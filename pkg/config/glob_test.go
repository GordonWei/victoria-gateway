package config

import "testing"

func TestValidateHybrid_BadGlob(t *testing.T) {
	c := validHybridConfig()
	c.HybridRoutes[0].Matchers = map[string]string{"host": "web-[0-9"}
	wantErrContaining(t, c.Validate(), "hybrid_routes[0]: matcher \"host\"")
}

func TestValidate_NotificationsBadGlob(t *testing.T) {
	c := validConfig()
	c.Notifications = validNotifications()
	c.Notifications.Routes = append([]NotifyRouteConfig{{Matchers: map[string]string{"host": "[oops"}, Channels: []string{c.Notifications.Channels[0].Name}}}, c.Notifications.Routes...)
	wantErrContaining(t, c.Validate(), "notifications.routes[0]: matcher \"host\"")
}

func TestValidate_MaintenanceBadGlob(t *testing.T) {
	err := ValidateMaintenanceWindows([]MaintenanceWindow{{Name: "w", Schedule: "DAILY 01:00-02:00", Matchers: map[string]string{"host": "db-[1-3"}, Action: "mute"}})
	wantErrContaining(t, err, "maintenance_windows[0] (w): matcher \"host\"")
}
