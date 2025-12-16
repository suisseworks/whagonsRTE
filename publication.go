package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/gorilla/websocket"
	"github.com/lib/pq"
)

// startPublicationListeners starts listeners for all tenant databases
func (e *RealtimeEngine) startPublicationListeners() {
	e.mutex.RLock()
	tenantDBs := make(map[string]*sql.DB)
	for name, db := range e.tenantDBs {
		tenantDBs[name] = db
	}
	e.mutex.RUnlock()

	// We need to get the actual database names for each tenant
	query := "SELECT name, database FROM tenants WHERE database IS NOT NULL"
	rows, err := e.landlordDB.Query(query)
	if err != nil {
		log.Printf("❌ Failed to query tenants for listeners: %v", err)
		return
	}
	defer rows.Close()

	for rows.Next() {
		var tenantName, dbName string
		if err := rows.Scan(&tenantName, &dbName); err != nil {
			log.Printf("⚠️  Error scanning tenant row for listener: %v", err)
			continue
		}

		if _, exists := tenantDBs[tenantName]; exists {
			go e.listenToTenantPublications(tenantName, dbName)
		}
	}
}

// listenToTenantPublications listens to PostgreSQL notifications for a specific tenant
func (e *RealtimeEngine) listenToTenantPublications(tenantName, dbName string) {
	log.Printf("🎧 Starting publication listener for tenant: %s (database: %s)", tenantName, dbName)

	listener := pq.NewListener(
		fmt.Sprintf("host=%s port=%s user=%s password=%s dbname=%s sslmode=disable",
			config.DBHost, config.DBPort, config.DBUsername, config.DBPassword, dbName),
		10*time.Second,
		time.Minute,
		func(ev pq.ListenerEventType, err error) {
			if err != nil {
				log.Printf("❌ PostgreSQL listener error for %s: %v", tenantName, err)
			}
		})

	defer listener.Close()

	// Query all tables with change triggers dynamically
	// Get a connection to the tenant database to query triggers
	e.mutex.RLock()
	tenantDB, exists := e.tenantDBs[tenantName]
	e.mutex.RUnlock()

	if !exists {
		log.Printf("❌ Tenant database connection not found for %s", tenantName)
		return
	}

	// Query for all triggers that match the pattern *_changes_trigger
	// This dynamically finds all tables (wh_* and Spatie permission tables) with triggers
	query := `
		SELECT DISTINCT 
			REPLACE(tgname, '_changes_trigger', '') as table_name
		FROM pg_trigger 
		WHERE tgname LIKE '%_changes_trigger'
		AND tgisinternal = false
		ORDER BY table_name
	`

	rows, err := tenantDB.Query(query)
	if err != nil {
		log.Printf("❌ Failed to query triggers for tenant %s: %v", tenantName, err)
		log.Printf("⚠️  No channels will be subscribed for tenant %s", tenantName)
		return
	}
	defer rows.Close()

	var channels []string
	for rows.Next() {
		var tableName string
		if err := rows.Scan(&tableName); err != nil {
			log.Printf("⚠️  Error scanning trigger row: %v", err)
			continue
		}
		channelName := fmt.Sprintf("whagons_%s_changes", tableName)
		channels = append(channels, channelName)
	}

	if len(channels) == 0 {
		log.Printf("⚠️  No triggers found for tenant %s - no channels will be subscribed", tenantName)
		return
	}

	// Subscribe to all channels dynamically discovered
	for _, channelName := range channels {
		if err := listener.Listen(channelName); err != nil {
			log.Printf("⚠️  Failed to listen to channel %s for tenant %s: %v", channelName, tenantName, err)
			continue
		}
		log.Printf("✅ Listening to channel '%s' for tenant: %s", channelName, tenantName)
	}

	log.Printf("📡 Subscribed to %d channels for tenant: %s", len(channels), tenantName)

	for {
		select {
		case notification := <-listener.Notify:
			if notification != nil {
				e.handlePublicationNotification(tenantName, notification)
			}
		case <-time.After(90 * time.Second):
			// Ping to keep connection alive
			if err := listener.Ping(); err != nil {
				log.Printf("❌ Ping failed for tenant %s: %v", tenantName, err)
				return
			}
		}
	}
}

// handlePublicationNotification processes a PostgreSQL notification
func (e *RealtimeEngine) handlePublicationNotification(tenantName string, notification *pq.Notification) {
	log.Printf("📡 Publication notification received from %s on channel '%s'", tenantName, notification.Channel)

	// Parse the PostgreSQL notification payload once
	var pgNotification PostgreSQLNotification
	if err := json.Unmarshal([]byte(notification.Extra), &pgNotification); err != nil {
		log.Printf("❌ Failed to parse notification JSON from %s: %v", tenantName, err)
		return
	}

	// Create clean publication message with raw JSON data (generic for all tables)
	message := PublicationMessage{
		Type:        "database",
		TenantName:  tenantName,
		Table:       pgNotification.Table,
		Operation:   pgNotification.Operation,
		NewData:     pgNotification.NewData,
		OldData:     pgNotification.OldData,
		DBTimestamp: pgNotification.Timestamp,
		ClientTime:  time.Now().Format(time.RFC3339),
	}

	// Generate a generic message based on operation
	switch pgNotification.Operation {
	case "INSERT":
		message.Message = fmt.Sprintf("New record created in %s.%s", tenantName, pgNotification.Table)
	case "UPDATE":
		message.Message = fmt.Sprintf("Record updated in %s.%s", tenantName, pgNotification.Table)
	case "DELETE":
		message.Message = fmt.Sprintf("Record deleted from %s.%s", tenantName, pgNotification.Table)
	default:
		message.Message = fmt.Sprintf("%s operation on %s.%s", pgNotification.Operation, tenantName, pgNotification.Table)
	}

	log.Printf("🔄 Processed %s operation on %s.%s - broadcasting to sessions",
		pgNotification.Operation, tenantName, pgNotification.Table)

	// Broadcast to all connected WebSocket sessions
	e.BroadcastPublicationMessage(message)
}

// BroadcastPublicationMessage sends a publication message to authenticated sessions with tenant access
func (e *RealtimeEngine) BroadcastPublicationMessage(message PublicationMessage) {
	e.mutex.RLock()
	sessions := make(map[string]*WebSocketSession)
	authSessions := make(map[string]*AuthenticatedSession)
	for id, session := range e.sessions {
		sessions[id] = session
	}
	for id, authSession := range e.authenticatedSessions {
		authSessions[id] = authSession
	}
	e.mutex.RUnlock()

	broadcastCount := 0
	authorizedCount := 0

	for sessionID, wsSession := range sessions {
		authSession, isAuthenticated := authSessions[sessionID]

		if !isAuthenticated {
			// Skip unauthenticated sessions (shouldn't happen with new auth flow)
			log.Printf("⚠️ Skipping unauthenticated session %s", sessionID)
			continue
		}

		// Check if the authenticated session can access this tenant's data
		if !authSession.canAccessTenant(message.TenantName) {
			log.Printf("🔒 Session %s (tenant: %s) denied access to %s data",
				sessionID, authSession.TenantName, message.TenantName)
			continue
		}

		authorizedCount++

		// Set the sessionId for this specific session
		message.SessionId = sessionID

		jsonMessage, err := json.Marshal(message)
		if err != nil {
			log.Printf("❌ Failed to marshal publication message: %v", err)
			continue
		}

		wsSession.Conn.SetWriteDeadline(time.Now().Add(writeWait))
		if err := wsSession.Conn.WriteMessage(websocket.TextMessage, jsonMessage); err != nil {
			log.Printf("❌ Failed to send to session %s: %v", sessionID, err)
			// Remove failed session
			e.mutex.Lock()
			delete(e.sessions, sessionID)
			delete(e.authenticatedSessions, sessionID)
			e.mutex.Unlock()
		} else {
			broadcastCount++
			log.Printf("📤 Sent publication to authenticated session %s (tenant: %s)",
				sessionID, authSession.TenantName)
		}
	}

	if authorizedCount > 0 {
		log.Printf("📡 Broadcasted publication to %d/%d authorized sessions for tenant: %s",
			broadcastCount, authorizedCount, message.TenantName)
	} else {
		log.Printf("📡 No authorized sessions found for tenant: %s", message.TenantName)
	}
}
