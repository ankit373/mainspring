package config

import "testing"

func TestTLSEnabled(t *testing.T) {
	if (Config{}).TLSEnabled() {
		t.Fatal("no cert/key => TLS disabled")
	}
	if (Config{TLSCert: "c.pem"}).TLSEnabled() {
		t.Fatal("cert without key => TLS disabled")
	}
	if (Config{TLSKey: "k.pem"}).TLSEnabled() {
		t.Fatal("key without cert => TLS disabled")
	}
	if !(Config{TLSCert: "c.pem", TLSKey: "k.pem"}).TLSEnabled() {
		t.Fatal("cert+key => TLS enabled")
	}
}
