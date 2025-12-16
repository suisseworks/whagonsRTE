-- PostgreSQL notification setup for landlord database tenants table
-- This script sets up triggers to notify whagonsRTE when tenants are added, updated, or deleted

-- Create notification function
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
    
    -- Log the notification for debugging
    RAISE NOTICE 'Tenant notification sent: %', payload;
    
    RETURN COALESCE(NEW, OLD);
END;
$$ LANGUAGE plpgsql;

-- Create triggers for tenants table
DROP TRIGGER IF EXISTS tenant_changes_trigger ON tenants;

CREATE TRIGGER tenant_changes_trigger
    AFTER INSERT OR UPDATE OR DELETE
    ON tenants
    FOR EACH ROW
    EXECUTE FUNCTION notify_tenant_changes();

-- Grant necessary permissions (adjust schema/user as needed)
-- GRANT USAGE ON SCHEMA public TO your_app_user;
-- GRANT SELECT ON tenants TO your_app_user;

-- Test the setup (optional - remove in production)
-- INSERT INTO tenants (name, domain, database) VALUES ('test_tenant', 'test.example.com', 'test_db');
-- UPDATE tenants SET domain = 'updated.example.com' WHERE name = 'test_tenant';
-- DELETE FROM tenants WHERE name = 'test_tenant';

COMMENT ON FUNCTION notify_tenant_changes() IS 'Sends PostgreSQL notifications when tenants table changes for whagonsRTE real-time sync';
COMMENT ON TRIGGER tenant_changes_trigger ON tenants IS 'Triggers tenant change notifications for whagonsRTE'; 