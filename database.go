package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/lib/pq"
	_ "github.com/lib/pq"
)

// connectToLandlord establishes connection to the landlord database
func (e *RealtimeEngine) connectToLandlord() error {
	connStr := fmt.Sprintf("host=%s port=%s user=%s password=%s dbname=%s sslmode=disable",
		config.DBHost, config.DBPort, config.DBUsername, config.DBPassword, config.DBLandlord)

	db, err := sql.Open("postgres", connStr)
	if err != nil {
		return fmt.Errorf("failed to open landlord database: %w", err)
	}

	// Configure connection pool - shared by all clients for tenant lookups
	db.SetMaxOpenConns(10)                 // Handle more concurrent tenant lookups across all domains
	db.SetMaxIdleConns(4)                  // Keep more connections alive for quick lookups
	db.SetConnMaxLifetime(5 * time.Minute) // Recycle connections periodically

	if err := db.Ping(); err != nil {
		return fmt.Errorf("failed to ping landlord database: %w", err)
	}

	e.landlordDB = db
	log.Println("✅ Connected to landlord database")

	// Set up tenant notification system
	if err := e.setupTenantNotifications(); err != nil {
		log.Printf("⚠️  Failed to setup tenant notifications: %v", err)
		log.Println("🔍 Tenant changes will not be detected automatically")
	} else {
		log.Println("✅ Tenant notification system ready")
	}

	return nil
}

// setupTenantNotifications creates the PostgreSQL trigger system for tenant change notifications
func (e *RealtimeEngine) setupTenantNotifications() error {
	log.Println("🔧 Setting up tenant notification system...")

	// Create the notification function
	createFunctionSQL := `
		CREATE OR REPLACE FUNCTION notify_tenant_changes()
		RETURNS TRIGGER AS $$
		DECLARE
			payload JSON;
		BEGIN
			-- Build notification payload
			IF TG_OP = 'DELETE' THEN
				payload = json_build_object(
					'operation', TG_OP,
					'table', TG_TABLE_NAME,
					'old_data', row_to_json(OLD),
					'timestamp', extract(epoch from now())
				);
			ELSE
				payload = json_build_object(
					'operation', TG_OP,
					'table', TG_TABLE_NAME,
					'new_data', row_to_json(NEW),
					'old_data', CASE WHEN TG_OP = 'UPDATE' THEN row_to_json(OLD) ELSE NULL END,
					'timestamp', extract(epoch from now())
				);
			END IF;

			-- Send notification
			PERFORM pg_notify('tenant_changes', payload::text);
			
			RETURN COALESCE(NEW, OLD);
		END;
		$$ LANGUAGE plpgsql;`

	if _, err := e.landlordDB.Exec(createFunctionSQL); err != nil {
		return fmt.Errorf("failed to create notification function: %w", err)
	}

	// Drop existing trigger if it exists
	dropTriggerSQL := `DROP TRIGGER IF EXISTS tenant_changes_trigger ON tenants;`
	if _, err := e.landlordDB.Exec(dropTriggerSQL); err != nil {
		return fmt.Errorf("failed to drop existing trigger: %w", err)
	}

	// Create the trigger
	createTriggerSQL := `
		CREATE TRIGGER tenant_changes_trigger
			AFTER INSERT OR UPDATE OR DELETE
			ON tenants
			FOR EACH ROW
			EXECUTE FUNCTION notify_tenant_changes();`

	if _, err := e.landlordDB.Exec(createTriggerSQL); err != nil {
		return fmt.Errorf("failed to create trigger: %w", err)
	}

	log.Println("✅ Tenant notification function and trigger created")
	return nil
}

// loadTenantDatabases queries the landlord database for tenant information and connects to each tenant database
func (e *RealtimeEngine) loadTenantDatabases() error {
	query := "SELECT id, name, domain, database FROM tenants WHERE database IS NOT NULL"
	rows, err := e.landlordDB.Query(query)
	if err != nil {
		return fmt.Errorf("failed to query tenants: %w", err)
	}
	defer rows.Close()

	var tenants []TenantDB
	for rows.Next() {
		var tenant TenantDB
		if err := rows.Scan(&tenant.ID, &tenant.Name, &tenant.Domain, &tenant.Database); err != nil {
			log.Printf("⚠️  Error scanning tenant row: %v", err)
			continue
		}
		tenants = append(tenants, tenant)
	}

	log.Printf("📊 Found %d tenant databases", len(tenants))

	// Connect to each tenant database
	for _, tenant := range tenants {
		if err := e.connectToTenant(tenant); err != nil {
			log.Printf("⚠️  Failed to connect to tenant %s: %v", tenant.Name, err)
			continue
		}
		log.Printf("✅ Connected to tenant database: %s", tenant.Name)
	}

	return nil
}

// reloadTenantDatabases checks for new tenants and connects to them
func (e *RealtimeEngine) reloadTenantDatabases() error {
	query := "SELECT id, name, domain, database FROM tenants WHERE database IS NOT NULL"
	rows, err := e.landlordDB.Query(query)
	if err != nil {
		return fmt.Errorf("failed to query tenants: %w", err)
	}
	defer rows.Close()

	var tenants []TenantDB
	for rows.Next() {
		var tenant TenantDB
		if err := rows.Scan(&tenant.ID, &tenant.Name, &tenant.Domain, &tenant.Database); err != nil {
			log.Printf("⚠️  Error scanning tenant row: %v", err)
			continue
		}
		tenants = append(tenants, tenant)
	}

	// Check for new tenants that aren't already connected
	e.mutex.RLock()
	existingTenants := make(map[string]bool)
	for name := range e.tenantDBs {
		existingTenants[name] = true
	}
	e.mutex.RUnlock()

	newTenantsCount := 0
	for _, tenant := range tenants {
		if !existingTenants[tenant.Name] {
			if err := e.connectToTenant(tenant); err != nil {
				log.Printf("⚠️  Failed to connect to new tenant %s: %v", tenant.Name, err)
				continue
			}
			log.Printf("✅ Connected to new tenant database: %s", tenant.Name)

			// Start publication listener for the new tenant
			go e.listenToTenantPublications(tenant.Name, tenant.Database)
			newTenantsCount++
		}
	}

	if newTenantsCount > 0 {
		log.Printf("🆕 Connected to %d new tenant(s)", newTenantsCount)
	} else {
		log.Println("🔍 No new tenants found")
	}

	return nil
}

// ReloadTenants checks for new tenants and connects to them (implements RealtimeEngineInterface)
func (e *RealtimeEngine) ReloadTenants() error {
	if e.landlordDB == nil {
		return fmt.Errorf("landlord database not connected")
	}
	return e.reloadTenantDatabases()
}

// TestTenantNotification sends a test notification to verify the landlord listener is working (implements RealtimeEngineInterface)
func (e *RealtimeEngine) TestTenantNotification() error {
	if e.landlordDB == nil {
		return fmt.Errorf("landlord database not connected")
	}

	testQuery := `SELECT pg_notify('tenant_changes', '{"operation":"MANUAL_TEST","table":"tenants","message":"Manual test from API endpoint","timestamp":' || extract(epoch from now()) || '}')`
	if _, err := e.landlordDB.Exec(testQuery); err != nil {
		return fmt.Errorf("failed to send test notification: %w", err)
	}

	log.Println("🧪 Manual test notification sent via API")
	return nil
}

// listenToLandlordTenantChanges listens for changes to the tenants table in the landlord database
func (e *RealtimeEngine) listenToLandlordTenantChanges() {
	log.Println("🎧 Starting landlord tenant changes listener")

	connStr := fmt.Sprintf("host=%s port=%s user=%s password=%s dbname=%s sslmode=disable",
		config.DBHost, config.DBPort, config.DBUsername, config.DBPassword, config.DBLandlord)

	listener := pq.NewListener(
		connStr,
		10*time.Second,
		time.Minute,
		func(ev pq.ListenerEventType, err error) {
			if err != nil {
				log.Printf("❌ Landlord tenant listener error: %v", err)
			} else {
				log.Printf("🔍 Landlord listener event: %v", ev)
			}
		})

	defer listener.Close()

	// Listen to the tenants table changes channel
	channelName := "tenant_changes"
	if err := listener.Listen(channelName); err != nil {
		log.Printf("❌ Failed to listen to landlord channel %s: %v", channelName, err)
		return
	}

	log.Printf("✅ Listening to landlord channel '%s' for tenant changes", channelName)

	// Send a test notification to verify the connection is working
	go func() {
		time.Sleep(2 * time.Second) // Wait a bit for listener to be ready
		testQuery := `SELECT pg_notify('tenant_changes', '{"operation":"CONNECTION_TEST","table":"tenants","message":"whagonsRTE listener connection test","timestamp":' || extract(epoch from now()) || '}')`
		if _, err := e.landlordDB.Exec(testQuery); err != nil {
			log.Printf("⚠️  Failed to send test notification: %v", err)
		} else {
			log.Println("🧪 Sent test notification to verify listener connection")
		}
	}()

	pingCount := 0
	for {
		select {
		case notification := <-listener.Notify:
			if notification != nil {
				e.handleTenantChangeNotification(notification)
			} else {
				log.Println("⚠️  Received nil notification from landlord listener")
			}
		case <-time.After(90 * time.Second):
			// Ping to keep connection alive
			if err := listener.Ping(); err != nil {
				log.Printf("❌ Landlord listener ping failed: %v", err)
				return
			}
			pingCount++
			if pingCount%5 == 0 { // Log every 5th ping (every ~7.5 minutes)
				log.Printf("💓 Landlord listener still alive (ping #%d)", pingCount)
			}
		}
	}
}

// handleTenantChangeNotification processes notifications from the landlord tenants table
func (e *RealtimeEngine) handleTenantChangeNotification(notification *pq.Notification) {
	log.Printf("🔔 Received tenant change notification on channel '%s'", notification.Channel)

	if notification.Extra != "" {
		var payload struct {
			Operation string    `json:"operation"`
			Table     string    `json:"table"`
			NewData   *TenantDB `json:"new_data"`
			OldData   *TenantDB `json:"old_data"`
			Timestamp float64   `json:"timestamp"`
		}

		if err := json.Unmarshal([]byte(notification.Extra), &payload); err != nil {
			log.Printf("⚠️  Failed to parse tenant notification payload: %v", err)
			// Fallback to full reload
			if err := e.reloadTenantDatabases(); err != nil {
				log.Printf("⚠️  Failed to reload tenants after notification: %v", err)
			}
			return
		}

		log.Printf("🔄 Tenant %s operation: %s", payload.Table, payload.Operation)

		// Handle test notifications
		if payload.Operation == "CONNECTION_TEST" {
			log.Println("✅ Landlord listener connection test successful!")
			return
		}
		if payload.Operation == "MANUAL_TEST" {
			log.Println("✅ Manual test notification received successfully!")
			return
		}

		switch payload.Operation {
		case "INSERT":
			if payload.NewData != nil && payload.NewData.Database != "" {
				log.Printf("➕ New tenant detected: %s (database: %s)", payload.NewData.Name, payload.NewData.Database)
				// Connect to new tenant with retry logic (database might not exist yet)
				go e.connectToTenantWithRetry(*payload.NewData)
			}
		case "UPDATE":
			if payload.NewData != nil {
				log.Printf("🔄 Tenant updated: %s", payload.NewData.Name)
				// For updates, do a full reload to handle database name changes, etc.
				if err := e.reloadTenantDatabases(); err != nil {
					log.Printf("⚠️  Failed to reload tenants after update: %v", err)
				}
			}
		case "DELETE":
			if payload.OldData != nil {
				log.Printf("➖ Tenant deleted: %s", payload.OldData.Name)
				// Close connection to deleted tenant
				e.mutex.Lock()
				if db, exists := e.tenantDBs[payload.OldData.Name]; exists {
					if err := db.Close(); err != nil {
						log.Printf("⚠️  Error closing deleted tenant database %s: %v", payload.OldData.Name, err)
					}
					delete(e.tenantDBs, payload.OldData.Name)
					log.Printf("🗑️  Disconnected from deleted tenant: %s", payload.OldData.Name)
				}
				e.mutex.Unlock()
			}
		default:
			log.Printf("❓ Unknown tenant operation: %s", payload.Operation)
		}
	} else {
		// No payload, do full reload
		log.Println("🔄 Tenant notification without payload, performing full reload")
		if err := e.reloadTenantDatabases(); err != nil {
			log.Printf("⚠️  Failed to reload tenants after notification: %v", err)
		}
	}
}

// connectToTenant establishes connection to a specific tenant database
func (e *RealtimeEngine) connectToTenant(tenant TenantDB) error {
	connStr := fmt.Sprintf("host=%s port=%s user=%s password=%s dbname=%s sslmode=disable",
		config.DBHost, config.DBPort, config.DBUsername, config.DBPassword, tenant.Database)

	db, err := sql.Open("postgres", connStr)
	if err != nil {
		return fmt.Errorf("failed to open tenant database %s: %w", tenant.Database, err)
	}

	// Configure connection pool - shared by all clients of this tenant
	db.SetMaxOpenConns(30)                 // Safe max: allows ~3 tenants within PostgreSQL 100-conn limit
	db.SetMaxIdleConns(10)                 // Keep more connections alive for burst traffic
	db.SetConnMaxLifetime(5 * time.Minute) // Recycle connections periodically

	if err := db.Ping(); err != nil {
		return fmt.Errorf("failed to ping tenant database %s: %w", tenant.Database, err)
	}

	e.mutex.Lock()
	e.tenantDBs[tenant.Name] = db
	e.mutex.Unlock()

	return nil
}

// connectToTenantWithRetry attempts to connect to a tenant database with retry logic
func (e *RealtimeEngine) connectToTenantWithRetry(tenant TenantDB) {
	maxRetries := 5
	baseDelay := 2 * time.Second

	for attempt := 1; attempt <= maxRetries; attempt++ {
		// Check if we're already connected to avoid duplicate connections
		e.mutex.RLock()
		_, alreadyConnected := e.tenantDBs[tenant.Name]
		e.mutex.RUnlock()

		if alreadyConnected {
			log.Printf("🔗 Tenant %s already connected, skipping retry", tenant.Name)
			return
		}

		log.Printf("🔄 Attempting to connect to tenant %s (attempt %d/%d)", tenant.Name, attempt, maxRetries)

		if err := e.connectToTenant(tenant); err != nil {
			if attempt == maxRetries {
				log.Printf("❌ Failed to connect to tenant %s after %d attempts: %v", tenant.Name, maxRetries, err)
				return
			}

			// Calculate exponential backoff delay
			delay := time.Duration(attempt) * baseDelay
			log.Printf("⏱️  Connection failed, retrying in %v... (error: %v)", delay, err)
			time.Sleep(delay)
			continue
		}

		// Success!
		log.Printf("✅ Connected to new tenant: %s (attempt %d)", tenant.Name, attempt)

		// Start publication listener for the new tenant
		go e.listenToTenantPublications(tenant.Name, tenant.Database)
		return
	}
}

// closeDatabases closes all database connections gracefully
func (e *RealtimeEngine) closeDatabases() {
	e.mutex.Lock()
	defer e.mutex.Unlock()

	// Close tenant databases
	for name, db := range e.tenantDBs {
		if err := db.Close(); err != nil {
			log.Printf("⚠️  Error closing tenant database %s: %v", name, err)
		} else {
			log.Printf("✅ Closed tenant database: %s", name)
		}
	}

	// Close landlord database
	if e.landlordDB != nil {
		if err := e.landlordDB.Close(); err != nil {
			log.Printf("⚠️  Error closing landlord database: %v", err)
		} else {
			log.Println("✅ Closed landlord database")
		}
	}
}
