package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// Discovery (-discover): read-only. Finds how PERCo-Web accepts a block with an end date: saves the API description
// (api-docs) next to the report, lists the methods about blocking, the planner and end dates, and shows which
// block/date fields a test employee's card has. Nothing is written to PERCo.

type rawGetter interface {
	GetRaw(ctx context.Context, rootPath string) ([]byte, int, error)
}

var docCandidates = []string{"/api-docs", "/api/api-docs", "/api-docs/swagger.json", "/api/swagger.json", "/swagger.json", "/api/docs"}

// discoverKeys are the words that make a method or a field relevant to "block until a date".
var discoverKeys = []string{"block", "planner", "plan", "end_date", "enddate", "date_end", "dateend", "expire"}

var specLink = regexp.MustCompile(`["']([^"'\s]+\.(?:json|ya?ml))["']`)

func discover(ctx context.Context, g rawGetter, staffID int64, dir string, out *log.Logger) int {
	spec, from := findSpec(ctx, g, out)
	if spec == nil {
		out.Printf("описание API не найдено по адресам %v — пришлите адрес, по которому в браузере открывается api-docs", docCandidates)
	} else {
		file := filepath.Join(dir, "perco-api-docs.json")
		if err := os.WriteFile(file, spec, 0o644); err == nil {
			out.Printf("описание API (%s) сохранено: %s — пришлите этот файл", from, file)
		}
		listRelevant(spec, out)
	}
	inspectStaff(ctx, g, staffID, out)
	return 0
}

// findSpec tries the usual addresses; a Swagger UI page is followed to the JSON it loads.
func findSpec(ctx context.Context, g rawGetter, out *log.Logger) ([]byte, string) {
	tried := map[string]bool{}
	queue := slices.Clone(docCandidates)
	for len(queue) > 0 && len(tried) < 12 {
		p := queue[0]
		queue = queue[1:]
		if tried[p] {
			continue
		}
		tried[p] = true
		data, status, err := g.GetRaw(ctx, p)
		if err != nil || status != http.StatusOK {
			continue
		}
		var probe struct {
			Paths map[string]json.RawMessage `json:"paths"`
		}
		if json.Unmarshal(data, &probe) == nil && len(probe.Paths) > 0 {
			return data, p
		}
		for _, m := range specLink.FindAllStringSubmatch(string(data), -1) {
			link := m[1]
			if strings.HasPrefix(link, "http") {
				continue // another host: not followed
			}
			if strings.HasPrefix(link, "/") {
				queue = append(queue, link)
				continue
			}
			// relative to the page: the page may be a folder (/api-docs/) or a file (/api-docs)
			queue = append(queue, path.Join(p, link), path.Join(path.Dir(p), link))
		}
	}
	return nil, ""
}

// listRelevant prints the methods whose path or description mention blocking, the planner or end dates,
// with the field names they take that look like dates or block flags.
func listRelevant(spec []byte, out *log.Logger) {
	var doc struct {
		Paths map[string]map[string]any `json:"paths"`
	}
	if err := json.Unmarshal(spec, &doc); err != nil {
		out.Printf("описание API не разобрано: %v", err)
		return
	}
	var lines []string
	for p, ops := range doc.Paths {
		for method, op := range ops {
			raw, _ := json.Marshal(op)
			text := strings.ToLower(p + " " + string(raw))
			if !containsAny(text, discoverKeys) {
				continue
			}
			summary := ""
			if m, ok := op.(map[string]any); ok {
				summary, _ = m["summary"].(string)
				if summary == "" {
					summary, _ = m["description"].(string)
				}
			}
			fields := relevantFields(op)
			line := fmt.Sprintf("%s %s — %s", strings.ToUpper(method), p, oneLine(summary, 120))
			if len(fields) > 0 {
				line += " | поля: " + strings.Join(fields, ", ")
			}
			lines = append(lines, line)
		}
	}
	slices.Sort(lines)
	out.Printf("методы API про блокировку / планировщик / даты окончания: %d", len(lines))
	for i, l := range lines {
		if i == 80 {
			out.Printf("… ещё %d, все — в файле perco-api-docs.json", len(lines)-80)
			break
		}
		out.Printf("  %s", l)
	}
}

// relevantFields collects names of properties and parameters that look like dates or block flags.
func relevantFields(v any) []string {
	set := map[string]bool{}
	var walk func(any)
	walk = func(v any) {
		switch t := v.(type) {
		case map[string]any:
			if name, ok := t["name"].(string); ok && looksRelevant(name) {
				set[name] = true
			}
			if props, ok := t["properties"].(map[string]any); ok {
				for k := range props {
					if looksRelevant(k) {
						set[k] = true
					}
				}
			}
			for _, x := range t {
				walk(x)
			}
		case []any:
			for _, x := range t {
				walk(x)
			}
		}
	}
	walk(v)
	var out []string
	for k := range set {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

func looksRelevant(name string) bool {
	n := strings.ToLower(name)
	return strings.Contains(n, "date") || strings.Contains(n, "block") || strings.Contains(n, "plan") || strings.Contains(n, "expire")
}

// inspectStaff shows the card's field names and the values of block/date/plan fields only — no name or photo.
func inspectStaff(ctx context.Context, g rawGetter, id int64, out *log.Logger) {
	data, status, err := g.GetRaw(ctx, "/api/users/staff/"+strconv.FormatInt(id, 10))
	if err != nil || status != http.StatusOK {
		out.Printf("карточка сотрудника %d: HTTP %d %v", id, status, err)
		return
	}
	var card map[string]json.RawMessage
	if err := json.Unmarshal(data, &card); err != nil {
		out.Printf("карточка сотрудника %d не разобрана: %v", id, err)
		return
	}
	var keys []string
	for k := range card {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	out.Printf("карточка сотрудника %d: поля %s", id, strings.Join(keys, ", "))
	for _, k := range keys {
		if looksRelevant(k) {
			out.Printf("  %s = %s", k, oneLine(string(card[k]), 300))
		}
	}
}

func containsAny(s string, words []string) bool {
	for _, w := range words {
		if strings.Contains(s, w) {
			return true
		}
	}
	return false
}

func oneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > max {
		return string(r[:max]) + "…"
	}
	return s
}
