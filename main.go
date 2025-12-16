package main

import (
	"database/sql"
	"log"
	"net/http"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/gorilla/websocket"
	_ "github.com/lib/pq"
	"github.com/suisseworks/whagonsRTE/routes"
)

func main() {
	engine := &RealtimeEngine{
		tenantDBs:             make(map[string]*sql.DB),
		sessions:              make(map[string]*WebSocketSession),
		authenticatedSessions: make(map[string]*AuthenticatedSession),
		tokenCache:            make(map[string]*CachedToken),
		upgrader: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool {
				// Allow all origins for development - be more restrictive in production
				return true
			},
			ReadBufferSize:  1024,
			WriteBufferSize: 1024,
		},
	}

	// Connect to landlord database
	if err := engine.connectToLandlord(); err != nil {
		log.Printf("⚠️  Failed to connect to landlord database: %v", err)
		log.Println("🔍 Application will start but database operations may fail")
	} else {
		defer engine.landlordDB.Close()

		// Load tenant databases
		if err := engine.loadTenantDatabases(); err != nil {
			log.Printf("⚠️  Failed to load tenant databases: %v", err)
			log.Println("🔍 Application will start but tenant operations may be limited")
		}
	}

	// Start listening to publications from tenant databases (only if we have database connections)
	if engine.landlordDB != nil && len(engine.tenantDBs) > 0 {
		go engine.startPublicationListeners()
	} else {
		log.Println("⚠️  Skipping publication listeners due to database connection issues")
	}

	// Start token cache cleanup routine
	go func() {
		ticker := time.NewTicker(5 * time.Minute) // Clean up every 5 minutes
		defer ticker.Stop()
		for range ticker.C {
			engine.cleanupExpiredTokens()
		}
	}()

	// Start zombie session cleanup routine
	go func() {
		ticker := time.NewTicker(30 * time.Second) // Clean up every 30 seconds
		defer ticker.Stop()
		for range ticker.C {
			engine.cleanupZombieSessions()
		}
	}()

	// Start listening for tenant changes in landlord database (only if landlord DB is connected)
	if engine.landlordDB != nil {
		go engine.listenToLandlordTenantChanges()
	}

	// Create Fiber app
	app := fiber.New(fiber.Config{
		ServerHeader: "WhagonsRTE",
		AppName:      "WhagonsRTE v1.0.0",
	})

	// Setup API routes with controllers
	routes.SetupRoutes(app, engine)

	// WebSocket endpoint with CORS support
	app.Get("/ws", func(c *fiber.Ctx) error {
		// Handle CORS preflight
		if c.Method() == "OPTIONS" {
			c.Set("Access-Control-Allow-Origin", "*")
			c.Set("Access-Control-Allow-Methods", "GET, OPTIONS")
			c.Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Requested-With, Accept, Origin, Cache-Control")
			c.Set("Access-Control-Allow-Credentials", "false")
			return c.SendStatus(200)
		}

		// Set CORS headers
		c.Set("Access-Control-Allow-Origin", "*")
		c.Set("Access-Control-Allow-Methods", "GET, OPTIONS")
		c.Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Requested-With, Accept, Origin, Cache-Control")
		c.Set("Access-Control-Allow-Credentials", "false")

		// Handle WebSocket upgrade
		return engine.websocketHandler(c)
	})

	// Server startup messages
	log.Printf("🚀 WhagonsRTE starting...")
	log.Printf("📡 Server listening on port: %s", config.ServerPort)
	log.Printf("🔌 WebSocket endpoint: ws://localhost:%s/ws", config.ServerPort)
	log.Printf("📊 API endpoints available:")
	log.Printf("   GET  /api/health - Health check")
	log.Printf("   GET  /api/metrics - System metrics")
	log.Printf("   GET  /api/sessions/count - Get connected sessions count")
	log.Printf("   POST /api/sessions/disconnect-all - Disconnect all sessions")
	log.Printf("   POST /api/tenants/reload - Reload and connect to new tenants")
	log.Printf("   POST /api/tenants/test-notification - Test tenant notification system")
	log.Printf("   POST /api/broadcast - Broadcast message to all sessions")

	// Start HTTP server with Fiber
	log.Fatal(app.Listen(":" + config.ServerPort))
}
