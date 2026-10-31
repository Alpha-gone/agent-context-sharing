package mcp

import (
	"encoding/json/v2"
	"fmt"
	"testing"
)

func TestBodyWhitespaceContract(t *testing.T) {
	catalog := mustCatalog(t)
	for _, tool := range []string{"node_create", "node_update"} {
		for i, body := range []string{"   ", "\n\t", "\u0085\u00a0\u2003\u3000", " surrounding text \n"} {
			t.Run(fmt.Sprintf("%s/%d", tool, i), func(t *testing.T) {
				args := catalogSample(tool)
				args["body"] = body
				data, err := json.Marshal(args)
				if err != nil {
					t.Fatal(err)
				}
				values, err := catalog.validate(tool, data)
				if err != nil || values["body"] != body {
					t.Fatalf("body not preserved: %v/%v", values["body"], err)
				}
			})
		}
		args := catalogSample(tool)
		args["body"] = ""
		validateCatalogSample(t, catalog, tool, args, "body")
	}
}
