# Implementation Plan: Subscribe to All wh_* Tables

## Goal
Update whagonsRTE to dynamically discover and subscribe to **all** `wh_*` tables with change triggers, instead of only subscribing to `wh_tasks`.

## Current State
- ✅ Currently hardcoded to subscribe only to `whagons_tasks_changes`
- ✅ Frontend already supports generic table handling via `CacheRegistry`
- ✅ Frontend expects `new_data` and `old_data` as generic JSON objects

## Changes Required

### 1. Update `types.go` - Make PublicationMessage Generic

**File**: `whagonsRTE/types.go`

**Change**: Update `PublicationMessage` struct to use `json.RawMessage` instead of `*TaskRecord`

```go
// PublicationMessage represents a clean publication message for the frontend
type PublicationMessage struct {
	Type        string          `json:"type"`
	TenantName  string          `json:"tenant_name"`
	Table       string          `json:"table"`
	Operation   string          `json:"operation"`
	NewData     json.RawMessage `json:"new_data,omitempty"`  // Changed from *TaskRecord
	OldData     json.RawMessage `json:"old_data,omitempty"`  // Changed from *TaskRecord
	Message     string          `json:"message"`
	DBTimestamp float64         `json:"db_timestamp"`
	ClientTime  string          `json:"client_timestamp"`
	SessionId   string          `json:"sessionId"`
}
```

**Note**: `TaskRecord` type can be kept for reference but won't be used in `PublicationMessage`.

---

### 2. Add `discoverNotifyChannels` Function

**File**: `whagonsRTE/publication.go`

**Add new function** after `listenToTenantPublications`:

```go
// discoverNotifyChannels queries the tenant DB for tables with change triggers
// and returns the corresponding NOTIFY channels (whagons_<table>_changes)
func discoverNotifyChannels(db *sql.DB) ([]string, error) {
	const q = `
		SELECT c.relname AS table_name
		FROM pg_trigger t
		JOIN pg_class c ON c.oid = t.tgrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE NOT t.tgisinternal
		  AND t.tgname = c.relname || '_changes_trigger'
		  AND n.nspname = 'public'
		  AND c.relname LIKE 'wh_%'
	`

	rows, err := db.Query(q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var channels []string
	for rows.Next() {
		var table string
		if err := rows.Scan(&table); err != nil {
			continue
		}
		channels = append(channels, fmt.Sprintf("whagons_%s_changes", table))
	}
	return channels, nil
}
```

**What it does**:
- Queries PostgreSQL system catalogs for all triggers matching pattern `*_changes_trigger`
- Filters to tables starting with `wh_` in the `public` schema
- Returns channel names in format `whagons_<table>_changes`

---

### 3. Update `listenToTenantPublications` - Dynamic Channel Discovery

**File**: `whagonsRTE/publication.go`

**Replace** lines 62-69 (hardcoded channel subscription) with:

```go
	// Discover all NOTIFY channels for this tenant by inspecting triggers
	e.mutex.RLock()
	tenantDB := e.tenantDBs[tenantName]
	e.mutex.RUnlock()
	if tenantDB == nil {
		log.Printf("❌ No tenant DB connection found for %s", tenantName)
		return
	}

	channels, err := discoverNotifyChannels(tenantDB)
	if err != nil {
		log.Printf("❌ Failed to discover notify channels for %s: %v", tenantName, err)
		return
	}
	if len(channels) == 0 {
		log.Printf("⚠️  No notify channels discovered for %s", tenantName)
	}

	// Subscribe to all discovered channels
	for _, ch := range channels {
		if err := listener.Listen(ch); err != nil {
			log.Printf("❌ Failed to listen to channel %s for tenant %s: %v", ch, tenantName, err)
			continue
		}
		log.Printf("✅ Listening to channel '%s' for tenant: %s", ch, tenantName)
	}
```

**What it does**:
- Dynamically discovers all `wh_*` tables with change triggers
- Subscribes to all discovered channels
- Logs each successful subscription

---

### 4. Update `handlePublicationNotification` - Generic Message Handling

**File**: `whagonsRTE/publication.go`

**Replace** lines 98-153 (task-specific parsing) with:

```go
	// Create generic publication message
	message := PublicationMessage{
		Type:        "database",
		TenantName:  tenantName,
		Table:       pgNotification.Table,
		Operation:   pgNotification.Operation,
		NewData:     pgNotification.NewData,  // Forward raw JSON directly
		OldData:     pgNotification.OldData,  // Forward raw JSON directly
		Message:     fmt.Sprintf("%s on %s.%s", pgNotification.Operation, tenantName, pgNotification.Table),
		DBTimestamp: pgNotification.Timestamp,
		ClientTime:  time.Now().Format(time.RFC3339),
	}
```

**Remove**:
- Task-specific parsing switch statement
- `getTaskName` helper function calls
- TaskRecord unmarshaling

**What it does**:
- Forwards raw JSON data directly to frontend
- Frontend's `CacheRegistry` handles routing by table name
- Generic message format works for any table

---

### 5. Remove `getTaskName` Helper Function

**File**: `whagonsRTE/publication.go`

**Delete** lines 162-168 (the `getTaskName` function):

```go
// getTaskName safely extracts the task name from a TaskRecord
func getTaskName(task *TaskRecord) string {
	if task == nil {
		return "unknown"
	}
	return task.Name
}
```

**Reason**: No longer needed since we're not parsing task-specific data.

---

## Expected Behavior After Changes

### On Startup
```
🎧 Starting publication listener for tenant: acme (database: whagons_acme)
✅ Listening to channel 'whagons_wh_tasks_changes' for tenant: acme
✅ Listening to channel 'whagons_wh_categories_changes' for tenant: acme
✅ Listening to channel 'whagons_wh_teams_changes' for tenant: acme
✅ Listening to channel 'whagons_wh_workspaces_changes' for tenant: acme
✅ Listening to channel 'whagons_wh_statuses_changes' for tenant: acme
... (30+ more channels)
```

### On Database Change
```
📡 Publication notification received from acme on channel 'whagons_wh_categories_changes'
🔄 Processed INSERT operation on acme.wh_categories - broadcasting to sessions
📤 Sent publication to authenticated session abc123 (tenant: acme)
```

### Message Format (Sent to Frontend)
```json
{
  "type": "database",
  "tenant_name": "acme",
  "table": "wh_categories",
  "operation": "INSERT",
  "new_data": {"id": 1, "name": "Support", "color": "#ff0000", ...},
  "old_data": null,
  "message": "INSERT on acme.wh_categories",
  "db_timestamp": 1704067200.123,
  "client_timestamp": "2024-01-01T12:00:00Z",
  "sessionId": "abc123"
}
```

---

## Testing Checklist

- [ ] Verify all `wh_*` tables with triggers are discovered
- [ ] Test INSERT operation on different tables (categories, teams, etc.)
- [ ] Test UPDATE operation on different tables
- [ ] Test DELETE operation on different tables
- [ ] Verify frontend receives and processes messages correctly
- [ ] Verify CacheRegistry routes messages to correct cache handlers
- [ ] Test with multiple tenants simultaneously
- [ ] Verify logs show all subscribed channels on startup

---

## Tables That Will Be Subscribed

Based on backend migrations, these tables have `_changes_trigger`:

**Core Entities**:
- `wh_tasks`, `wh_categories`, `wh_teams`, `wh_workspaces`
- `wh_users`, `wh_spots`, `wh_spot_types`, `wh_templates`
- `wh_priorities`, `wh_statuses`, `wh_status_transitions`

**Custom Fields**:
- `wh_custom_fields`, `wh_category_custom_field`
- `wh_spot_custom_fields`, `wh_template_custom_field`
- `wh_task_custom_field_value`, `wh_spot_custom_field_value`

**Relations & Tags**:
- `wh_tags`, `wh_task_tag`, `wh_user_team`
- `wh_category_priority`

**SLA & Permissions**:
- `wh_slas`, `wh_sla_alerts`, `wh_sla_policies`
- `wh_roles`, `wh_permissions`, `wh_role_permission`

**Forms**:
- `wh_forms`, `wh_form_fields`, `wh_form_versions`
- `wh_task_form`, `wh_field_options`

**Activity & Logs**:
- `wh_task_logs`, `wh_task_notes`, `wh_task_attachments`
- `wh_session_logs`, `wh_config_logs`

**Other**:
- `wh_approvals`, `wh_task_approval_instances`
- `wh_invitations`, `wh_exceptions`
- `wh_task_recurrences`, `wh_status_transition_logs`
- `wh_messages`, `wh_message_reads`
- `wh_job_positions`
- `wh_workflows`, `wh_workflow_versions`, `wh_workflow_nodes`, `wh_workflow_edges`, `wh_workflow_runs`, `wh_workflow_run_logs`

**Total**: 40+ tables will be automatically subscribed

---

## Rollback Plan

If issues occur, revert to hardcoded `wh_tasks` subscription:
1. Change `PublicationMessage.NewData` and `OldData` back to `*TaskRecord`
2. Replace dynamic discovery with: `channelName := "whagons_tasks_changes"`
3. Restore task-specific parsing in `handlePublicationNotification`

---

## Notes

- **No frontend changes needed** - Frontend already handles generic messages
- **Automatic discovery** - New tables with triggers are automatically picked up
- **Backward compatible** - Existing `wh_tasks` messages still work
- **Performance** - Multiple channel subscriptions are efficient (PostgreSQL LISTEN is lightweight)

