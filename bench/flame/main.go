// Command flame renders a flame graph SVG from `go tool pprof -traces` output,
// so profiles can be visualised without graphviz or third-party scripts.
//
//	go tool pprof -traces bin.test cpu.prof | go run ./bench/flame -title "CPU" > cpu.svg
//
// Each box is a function; its width is its share of samples, and boxes above
// it are its callees. Hover a box for the exact value.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"hash/fnv"
	"html"
	"io"
	"log"
	"os"
	"sort"
	"strconv"
	"strings"
)

type node struct {
	name     string
	value    float64
	children map[string]*node
}

func (n *node) child(name string) *node {
	if n.children == nil {
		n.children = map[string]*node{}
	}
	c := n.children[name]
	if c == nil {
		c = &node{name: name}
		n.children[name] = c
	}
	return c
}

func main() {
	title := flag.String("title", "Flame graph", "title")
	width := flag.Float64("width", 1200, "SVG width in pixels")
	flag.Parse()
	root, unit, err := parse(os.Stdin)
	if err != nil {
		log.Fatal(err)
	}
	if root.value == 0 {
		log.Fatal("no samples found in input")
	}
	render(os.Stdout, root, *title, unit, *width)
}

// parse reads pprof -traces blocks: a value on the first line with the leaf
// frame, then callers one per line, root last. Blocks are separated by
// "-----------+---" rules.
func parse(r io.Reader) (*node, string, error) {
	root := &node{name: "all"}
	var stack []string
	var value float64
	unit := ""
	flush := func() {
		if len(stack) == 0 {
			return
		}
		root.value += value
		n := root
		for i := len(stack) - 1; i >= 0; i-- { // root first
			n = n.child(stack[i])
			n.value += value
		}
		stack = stack[:0]
	}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "-----------+"):
			flush()
		case strings.HasPrefix(line, "       ") || strings.TrimSpace(line) == "":
			if f := strings.TrimSpace(line); f != "" && len(stack) > 0 {
				stack = append(stack, cleanFrame(f))
			}
		case len(line) > 0 && line[0] == ' ':
			fields := strings.Fields(line)
			if len(fields) < 2 {
				continue
			}
			v, u, ok := parseValue(fields[0])
			if !ok {
				continue // a label line such as "bytes:  1kB"
			}
			flush()
			value, unit = v, u
			stack = append(stack, cleanFrame(strings.Join(fields[1:], " ")))
		}
	}
	flush()
	return root, unit, sc.Err()
}

func cleanFrame(f string) string { return strings.TrimSuffix(f, " (inline)") }

// parseValue turns "3.97s", "120ms", "1.5MB" or "42" into a number in the
// smallest unit of its family (ns for time, B for bytes).
func parseValue(s string) (float64, string, bool) {
	units := []struct {
		suffix string
		mul    float64
		unit   string
	}{{"ns", 1, "ns"}, {"us", 1e3, "ns"}, {"µs", 1e3, "ns"}, {"ms", 1e6, "ns"}, {"s", 1e9, "ns"},
		{"GB", 1 << 30, "B"}, {"MB", 1 << 20, "B"}, {"kB", 1 << 10, "B"}, {"B", 1, "B"}}
	for _, u := range units {
		if strings.HasSuffix(s, u.suffix) {
			v, err := strconv.ParseFloat(strings.TrimSuffix(s, u.suffix), 64)
			return v * u.mul, u.unit, err == nil
		}
	}
	v, err := strconv.ParseFloat(s, 64)
	return v, "", err == nil
}

const rowH = 17

func depth(n *node) int {
	d := 0
	for _, c := range n.children {
		d = max(d, depth(c))
	}
	return d + 1
}

func render(w io.Writer, root *node, title, unit string, width float64) {
	rows := depth(root)
	height := float64(rows*rowH + 50)
	fmt.Fprintf(w, `<svg xmlns="http://www.w3.org/2000/svg" width="%.0f" height="%.0f" font-family="Menlo,monospace" font-size="11">`+"\n", width, height)
	fmt.Fprintf(w, `<rect width="100%%" height="100%%" fill="#fafafa"/><text x="%.0f" y="20" font-size="15" text-anchor="middle">%s</text>`+"\n", width/2, html.EscapeString(title))
	scale := (width - 20) / root.value
	var draw func(n *node, x float64, level int)
	draw = func(n *node, x float64, level int) {
		wpx := n.value * scale
		if wpx < 0.5 {
			return
		}
		y := height - float64((level+1)*rowH) - 8
		pct := 100 * n.value / root.value
		fmt.Fprintf(w, `<g><title>%s (%s, %.2f%%)</title><rect x="%.1f" y="%.0f" width="%.1f" height="%d" fill="%s" rx="2"/>`,
			html.EscapeString(n.name), format(n.value, unit), pct, x, y, wpx, rowH-1, color(n.name))
		if chars := int(wpx / 7); chars > 3 {
			label := n.name
			if len(label) > chars {
				label = label[:chars-2] + ".."
			}
			fmt.Fprintf(w, `<text x="%.1f" y="%.0f">%s</text>`, x+3, y+12, html.EscapeString(label))
		}
		fmt.Fprintln(w, "</g>")
		kids := make([]*node, 0, len(n.children))
		for _, c := range n.children {
			kids = append(kids, c)
		}
		sort.Slice(kids, func(i, j int) bool { return kids[i].name < kids[j].name })
		for _, c := range kids {
			draw(c, x, level+1)
			x += c.value * scale
		}
	}
	draw(root, 10, 0)
	fmt.Fprintln(w, "</svg>")
}

func format(v float64, unit string) string {
	switch unit {
	case "ns":
		return fmt.Sprintf("%.2fs", v/1e9)
	case "B":
		return fmt.Sprintf("%.1f MB", v/(1<<20))
	}
	return strconv.FormatFloat(v, 'f', 0, 64)
}

// color picks a warm colour per function, stable across renders; our own
// packages are tinted blue so they stand out.
func color(name string) string {
	h := fnv.New32a()
	h.Write([]byte(name))
	v := h.Sum32()
	if strings.Contains(name, "portcullis") {
		return fmt.Sprintf("rgb(%d,%d,%d)", 90+v%40, 150+v%50, 220+v%35)
	}
	return fmt.Sprintf("rgb(%d,%d,%d)", 205+v%50, 80+v%120, 40+v%50)
}
