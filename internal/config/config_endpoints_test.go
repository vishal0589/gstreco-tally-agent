package config

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestResolveTallyEndpoints_PrefersMultiList(t *testing.T) {
	c := &Config{
		TallyEndpoint:  "http://localhost:9000", // legacy single
		TallyEndpoints: []string{"http://localhost:9001", "http://localhost:9002"},
	}
	got := c.ResolveTallyEndpoints("http://localhost:9999")
	want := []string{"http://localhost:9001", "http://localhost:9002"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestResolveTallyEndpoints_FallsBackToLegacySingle(t *testing.T) {
	c := &Config{TallyEndpoint: "http://localhost:9000"}
	got := c.ResolveTallyEndpoints("http://default:9000")
	want := []string{"http://localhost:9000"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestResolveTallyEndpoints_FallsBackToFallbackWhenEmpty(t *testing.T) {
	c := &Config{}
	got := c.ResolveTallyEndpoints("http://default:9000")
	want := []string{"http://default:9000"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestResolveTallyEndpoints_DropsEmptyStringsInList(t *testing.T) {
	c := &Config{
		TallyEndpoints: []string{"  ", "http://localhost:9001", "", "  http://localhost:9002  "},
	}
	got := c.ResolveTallyEndpoints("")
	want := []string{"http://localhost:9001", "http://localhost:9002"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestResolveTallyEndpoints_NilConfig(t *testing.T) {
	var c *Config
	got := c.ResolveTallyEndpoints("http://default:9000")
	if len(got) != 1 || got[0] != "http://default:9000" {
		t.Errorf("nil config: got %v", got)
	}
}

func TestResolveTallyEndpoints_NilConfigNoFallback(t *testing.T) {
	var c *Config
	got := c.ResolveTallyEndpoints("")
	if got != nil {
		t.Errorf("expected nil, got %v", got)
	}
}

func TestSaveLoadRoundTrip_WithMultiEndpoints(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	want := &Config{
		Server:         "https://x",
		ConnectionID:   "id",
		TallyEndpoints: []string{"http://localhost:9000", "http://localhost:9001"},
	}
	if err := Save(path, want); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !reflect.DeepEqual(got.TallyEndpoints, want.TallyEndpoints) {
		t.Errorf("TallyEndpoints round-trip: got %v, want %v", got.TallyEndpoints, want.TallyEndpoints)
	}
}

func TestMergeEndpoints_KeepsPreviousAndDedupes(t *testing.T) {
	got := MergeEndpoints([]string{"http://localhost:9559", "http://127.0.0.1:3036"}, []string{"http://127.0.0.1:9559/", "http://127.0.0.1:9001"})
	want := []string{"http://127.0.0.1:3036", "http://127.0.0.1:9001", "http://localhost:9559"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("MergeEndpoints = %v, want %v", got, want)
	}
	if got := MergeEndpoints([]string{"http://127.0.0.1:9553"}, nil); len(got) != 1 {
		t.Fatalf("a miss removed an endpoint: %v", got)
	}
}

func TestRemoveEndpoints(t *testing.T) {
	got := RemoveEndpoints([]string{"http://localhost:9553", "http://127.0.0.1:9559"}, []string{"http://127.0.0.1:9553"})
	if len(got) != 1 || got[0] != "http://127.0.0.1:9559" {
		t.Fatalf("RemoveEndpoints = %v", got)
	}
}
