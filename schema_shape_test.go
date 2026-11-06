package main

import (
	"context"
	"encoding/json"
	"testing"
)

// Every property of every tool's input and output schema is a schema object.
// The MCP schema types each property as an object, and clients on the
// TypeScript SDK refuse the whole tool list when one is a boolean schema such
// as `true`. A Go field typed `any` gets that schema unless it carries a
// jsonschema description.
func TestToolSchemaPropertiesAreObjects(t *testing.T) {
	cs := connectFake(t, newFakeGitHub(), Config{})
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	sawWrite := false
	for _, tool := range res.Tools {
		if tool.Name == "pr_merge" {
			sawWrite = true
		}
		for kind, schema := range map[string]any{"input": tool.InputSchema, "output": tool.OutputSchema} {
			if schema == nil {
				continue
			}
			raw, err := json.Marshal(schema)
			if err != nil {
				t.Fatalf("%s %s schema: %v", tool.Name, kind, err)
			}
			var doc any
			if err := json.Unmarshal(raw, &doc); err != nil {
				t.Fatalf("%s %s schema: %v", tool.Name, kind, err)
			}
			checkProperties(t, tool.Name+" "+kind, doc)
		}
	}
	if !sawWrite {
		t.Fatal("the tool list lacks pr_merge, so the write tools went unchecked")
	}
}

// checkProperties walks a schema and reports any property schema that is not
// an object.
func checkProperties(t *testing.T, where string, node any) {
	t.Helper()
	m, ok := node.(map[string]any)
	if !ok {
		return
	}
	if props, ok := m["properties"].(map[string]any); ok {
		for name, p := range props {
			if _, ok := p.(map[string]any); !ok {
				t.Errorf("%s: property %q has schema %v, want an object", where, name, p)
				continue
			}
			checkProperties(t, where+"."+name, p)
		}
	}
	for _, key := range []string{"items", "additionalProperties"} {
		checkProperties(t, where+"["+key+"]", m[key])
	}
}
