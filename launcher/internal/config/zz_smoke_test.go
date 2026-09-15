package config

import "testing"

func TestSmokeValidateRealishConfig(t *testing.T) {
	good := []byte(`{
	  "schema": "kobra.launcher-config/1",
	  "game_id": "com.kobra.stardrifter",
	  "game_name": "Star Drifter",
	  "release": "2026.09.1",
	  "port": { "base": 8765, "span": 100 }
	}`)
	cfg, err := Parse(good)
	if err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	if cfg.Port.Base != 8765 || cfg.Port.Span != 100 {
		t.Fatalf("unexpected port window %+v", cfg.Port)
	}
	if cfg.DataAPI.MaxRequestBytes != 64<<20 {
		t.Fatalf("schema default not applied: %d", cfg.DataAPI.MaxRequestBytes)
	}
	if cfg.MimeTypes[".wasm"] != "application/wasm" {
		t.Fatalf("mime defaults not applied")
	}
	if cfg.Server.DrainTimeoutSeconds != 15 {
		t.Fatalf("server defaults not applied: %+v", cfg.Server)
	}
	bad := []byte(`{"schema":"kobra.launcher-config/1","game_id":"NotValid","game_name":"x","port":{"base":8765,"span":100}}`)
	if _, err := Parse(bad); err == nil {
		t.Fatal("bad game_id accepted")
	}
	bad2 := []byte(`{"schema":"kobra.launcher-config/1","game_id":"com.kobra.sd","game_name":"x","port":{"base":80,"span":100}}`)
	if _, err := Parse(bad2); err == nil {
		t.Fatal("privileged port base accepted")
	}
}
