package tools

import (
	"strings"
	"testing"
)

func TestRegistryDuplicateName(t *testing.T) {
	a, b := newDemoTool(t, "alpha"), newDemoTool(t, "alpha")
	_, err := NewRegistry(a, b)
	if err == nil {
		t.Fatal("duplicate names accepted")
	}
	if !strings.Contains(err.Error(), `duplicate tool name "alpha"`) {
		t.Errorf("error = %q, want duplicate tool name %q", err, "alpha")
	}
}

func TestRegistryGetMiss(t *testing.T) {
	reg, err := NewRegistry(newDemoTool(t, "alpha"))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := reg.Get("nope"); ok {
		t.Error("Get(unknown) reported ok=true")
	}
	if _, ok := reg.Get(""); ok {
		t.Error("Get(empty) reported ok=true")
	}
	if tl, ok := reg.Get("alpha"); !ok || tl == nil {
		t.Errorf("Get(alpha) = %v, %v", tl, ok)
	}
}

func TestRegistryAllOrder(t *testing.T) {
	reg, err := NewRegistry(
		newDemoTool(t, "gamma"),
		newDemoTool(t, "alpha"),
		newDemoTool(t, "beta"),
	)
	if err != nil {
		t.Fatal(err)
	}
	// Registration order is the contract — providers see a diffable list.
	want := []string{"gamma", "alpha", "beta"}
	all := reg.All()
	if len(all) != len(want) {
		t.Fatalf("All() = %d tools, want %d", len(all), len(want))
	}
	for i, name := range want {
		if all[i].Name() != name {
			t.Errorf("All()[%d] = %q, want %q", i, all[i].Name(), name)
		}
	}
}

func TestRegistryFuncTools(t *testing.T) {
	reg, err := NewRegistry(
		newDemoTool(t, "gamma"),
		newDemoTool(t, "alpha"),
		newDemoTool(t, "beta"),
	)
	if err != nil {
		t.Fatal(err)
	}
	fts := reg.FuncTools()
	if len(fts) != 3 {
		t.Fatalf("FuncTools() = %d, want 3", len(fts))
	}
	// Same order as All(), and the adapters carry the provider-facing name.
	want := []string{"gamma", "alpha", "beta"}
	for i, name := range want {
		if fts[i] == nil {
			t.Fatalf("FuncTools()[%d] is nil", i)
		}
		if fts[i].Name() != name {
			t.Errorf("FuncTools()[%d].Name() = %q, want %q", i, fts[i].Name(), name)
		}
	}
}

func TestBuiltinSetAndOrder(t *testing.T) {
	set, err := Builtin(Options{Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	// Seeded order mirrors registry.go Builtin; tools are appended, never
	// reordered, so provider tool lists stay diffable.
	want := []string{"read_csv", "csv_stats", "read_doc", "system_command", "sql_query",
		"make_dir", "list_dir", "download", "job_status"}
	if len(set) != len(want) {
		t.Fatalf("Builtin() = %d tools, want %d", len(set), len(want))
	}
	for i, name := range want {
		if set[i].Name() != name {
			t.Errorf("set[%d] = %q, want %q", i, set[i].Name(), name)
		}
		if set[i].Description() == "" {
			t.Errorf("tool %q has empty Description", name)
		}
		if set[i].FuncTool() == nil {
			t.Errorf("tool %q has nil FuncTool", name)
		}
	}
}
