package main

import (
	"database/sql"
	"encoding/json"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// TenantDB represents a tenant database configuration
type TenantDB struct {
	ID       int    `json:"id"`
	Name     string `json:"name"`
	Domain   string `json:"domain"`
	Database string `json:"database"`
}

// PostgreSQLNotification represents the notification payload from PostgreSQL triggers
type PostgreSQLNotification struct {
	Table     string          `json:"table"`
	Operation string          `json:"operation"`
	NewData   json.RawMessage `json:"new_data,omitempty"`
	OldData   json.RawMessage `json:"old_data,omitempty"`
	Timestamp float64         `json:"timestamp"`
}

// PublicationMessage represents a clean publication message for the frontend
type PublicationMessage struct {
	Type        string          `json:"type"`
	TenantName  string          `json:"tenant_name"`
	Table       string          `json:"table"`
	Operation   string          `json:"operation"`
	NewData     json.RawMessage `json:"new_data,omitempty"`
	OldData     json.RawMessage `json:"old_data,omitempty"`
	Message     string          `json:"message"`
	DBTimestamp float64         `json:"db_timestamp"`
	ClientTime  string          `json:"client_timestamp"`
	SessionId   string          `json:"sessionId"`
}

// SystemMessage represents system messages (connection, echo, etc.)
type SystemMessage struct {
	Type      string      `json:"type"`
	Operation string      `json:"operation"`
	Message   string      `json:"message"`
	Data      interface{} `json:"data,omitempty"`
	Timestamp string      `json:"timestamp"`
	SessionId string      `json:"sessionId"`
}

// WebSocketSession wraps a WebSocket connection with session metadata
type WebSocketSession struct {
	Conn     *websocket.Conn
	ID       string
	Tenant   string
	UserID   int
	LastPing time.Time
}

// RealtimeEngine is the main engine that manages database connections and WebSocket sessions
type RealtimeEngine struct {
	landlordDB            *sql.DB
	tenantDBs             map[string]*sql.DB
	sessions              map[string]*WebSocketSession     // Active WebSocket sessions
	authenticatedSessions map[string]*AuthenticatedSession // sessionID -> auth info
	tokenCache            map[string]*CachedToken          // tokenHash -> cached auth info
	mutex                 sync.RWMutex
	upgrader              websocket.Upgrader
}

// AuthenticatedSession represents an authenticated WebSocket session
type AuthenticatedSession struct {
	SessionID  string
	TenantName string
	UserID     int
	TokenID    int
	Abilities  []string
	ExpiresAt  *time.Time
	LastUsedAt time.Time
}

// PersonalAccessToken represents a Laravel Sanctum token from the database
type PersonalAccessToken struct {
	ID            int        `json:"id"`
	TokenableType string     `json:"tokenable_type"`
	TokenableID   int        `json:"tokenable_id"`
	Name          string     `json:"name"`
	Token         string     `json:"token"`
	Abilities     string     `json:"abilities"`
	LastUsedAt    *time.Time `json:"last_used_at"`
	ExpiresAt     *time.Time `json:"expires_at"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
}

// CachedToken represents a cached authentication result
type CachedToken struct {
	AuthSession *AuthenticatedSession
	ExpiresAt   time.Time
	Domain      string
}
