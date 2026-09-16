package catalog

import (
	"testing"
	"testing/fstest"
)

func workerDefaults() *Catalog {
	return &Catalog{Models: map[string]*Model{"qwen": {ID: "qwen", Vendor: "local", Protocol: "openai", API: APIEmbeddings, BaseURL: "http://127.0.0.1:18081/v1", NewConnectionPerRequest: true}}, Aliases: map[string]*Alias{"embedding": {Alias: "embedding", Chain: []string{"qwen"}}}}
}
func TestDefaultsAllowWorkerOnlyCatalogAndOperatorAliases(t *testing.T) {
	fs := fstest.MapFS{"models/.keep": {Data: nil}, "aliases/custom.yaml": {Data: []byte("alias: my-search\nchain: [qwen]\n")}}
	cat, err := loadFSDefaults(fs, workerDefaults())
	if err != nil {
		t.Fatal(err)
	}
	if len(cat.Models) != 1 || len(cat.Aliases) != 2 {
		t.Fatalf("unexpected catalog: %+v", cat)
	}
	if _, err := loadFS(fs); err == nil {
		t.Fatal("empty catalog accepted without defaults")
	}
}
func TestDefaultsRejectRouteAndAliasConflictsWithoutMutatingInput(t *testing.T) {
	for _, input := range []*Catalog{
		{Models: map[string]*Model{"qwen": {API: APIEmbeddings, BaseURL: "https://other"}}},
		{Aliases: map[string]*Alias{"embedding": {Chain: []string{"other"}}}},
		{Models: map[string]*Model{"embedding": {}}},
		{Aliases: map[string]*Alias{"qwen": {Chain: []string{"other"}}}},
	} {
		before := len(input.Models)
		if _, err := WithDefaults(input, workerDefaults()); err == nil {
			t.Fatal("accepted conflict")
		}
		if len(input.Models) != before {
			t.Fatal("mutated input")
		}
	}
	defaults := workerDefaults()
	if _, err := WithDefaults(defaults, workerDefaults()); err != nil {
		t.Fatal(err)
	}
}
