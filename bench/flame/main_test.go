package main

import (
	"bytes"
	"strings"
	"testing"
)

const sample = `File: x.test
-----------+-------------------------------------------------------
     3.00s   syscall.write
             net.(*conn).Write
             main.serve
-----------+-------------------------------------------------------
     1.00s   main.parse (inline)
             main.serve
-----------+-------------------------------------------------------
`

func TestParseBuildsTree(t *testing.T) {
	root, unit, err := parse(strings.NewReader(sample))
	if err != nil || unit != "ns" {
		t.Fatal(err, unit)
	}
	serve := root.children["main.serve"]
	if root.value != 4e9 || serve == nil || serve.value != 4e9 || serve.children["main.parse"].value != 1e9 ||
		serve.children["net.(*conn).Write"].children["syscall.write"].value != 3e9 {
		t.Fatalf("tree wrong: %+v", root)
	}
	var out bytes.Buffer
	render(&out, root, "t", unit, 800)
	if !strings.Contains(out.String(), "<svg") || !strings.Contains(out.String(), "syscall.write (3.00s, 75.00%)") {
		t.Fatalf("svg missing content:\n%s", out.String())
	}
}

func TestParseValue(t *testing.T) {
	for in, want := range map[string]float64{"10ms": 1e7, "2s": 2e9, "1.5kB": 1536, "7": 7} {
		if v, _, ok := parseValue(in); !ok || v != want {
			t.Errorf("parseValue(%q) = %v, want %v", in, v, want)
		}
	}
	if _, _, ok := parseValue("bytes:"); ok {
		t.Error("label parsed as a value")
	}
}
