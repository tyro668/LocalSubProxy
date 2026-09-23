package main

import "testing"

func TestRemoteServiceTokenEnvironmentMigration(t *testing.T) {
	t.Setenv("LOCALSUBPROXY_SAAS_TOKEN", "old-token")
	t.Setenv("LOCALSUBPROXY_REMOTE_TOKEN", "")
	if got := remoteServiceToken(); got != "old-token" {
		t.Fatalf("legacy token not accepted: %q", got)
	}
	t.Setenv("LOCALSUBPROXY_REMOTE_TOKEN", "new-token")
	if got := remoteServiceToken(); got != "new-token" {
		t.Fatalf("remote token not preferred: %q", got)
	}
}
