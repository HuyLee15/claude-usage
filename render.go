package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

const barWidth = 20

const (
	colorReset = "\x1b[0m"
	colorDim   = "\x1b[2m"
	colorRed   = "\x1b[31m"
)

// levelColors maps the shared utilization buckets to terminal colors.
var levelColors = map[string]string{
	"ok":   "\x1b[32m",
	"warn": "\x1b[33m",
	"crit": "\x1b[31m",
}

// level buckets utilization into the three colors both the table and the dashboard share.
func level(utilization float64) string {
	switch {
	case utilization >= 90:
		return "crit"
	case utilization >= 70:
		return "warn"
	}
	return "ok"
}

func colorEnabled() bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	info, err := os.Stdout.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

func renderTable(w io.Writer, results []result, color bool) {
	nameWidth, emailWidth := len("ACCOUNT"), len("EMAIL")
	for _, r := range results {
		nameWidth = max(nameWidth, len(r.Account.Name))
		emailWidth = max(emailWidth, len(r.Account.Email))
	}
	fmt.Fprintf(w, "%-*s  %-*s  %-*s  %-8s  %-*s  %-8s  %-6s  %-6s\n",
		nameWidth, "ACCOUNT", emailWidth, "EMAIL", barWidth+5, "5H", "RESETS", barWidth+5, "WEEK", "RESETS", "SONNET", "OPUS")
	for _, r := range results {
		fmt.Fprintf(w, "%-*s  %-*s  ", nameWidth, r.Account.Name, emailWidth, r.Account.Email)
		if r.Err != nil {
			fmt.Fprintln(w, paint(colorRed, r.Err.Error(), color))
			continue
		}
		fmt.Fprintf(w, "%s  %-8s  %s  %-8s  %-6s  %-6s\n",
			bar(r.Usage.FiveHour, color), resets(r.Usage.FiveHour),
			bar(r.Usage.SevenDay, color), resets(r.Usage.SevenDay),
			pct(r.Usage.SevenDaySonnet), pct(r.Usage.SevenDayOpus))
	}
	fmt.Fprintln(w, paint(colorDim, "updated "+time.Now().Format("15:04:05"), color))
}

func pct(w *window) string {
	if w == nil {
		return "—"
	}
	return fmt.Sprintf("%.0f%%", w.Utilization)
}

// bar renders a fixed-width "42% ████░░" cell; padding happens before painting so ANSI codes never skew columns.
func bar(w *window, color bool) string {
	if w == nil {
		return fmt.Sprintf("%-*s", barWidth+5, "—")
	}
	filled := min(max(int(w.Utilization/100*barWidth+0.5), 0), barWidth)
	cells := strings.Repeat("█", filled) + strings.Repeat("░", barWidth-filled)
	return paint(levelColors[level(w.Utilization)], fmt.Sprintf("%4s %s", pct(w), cells), color)
}

func resets(w *window) string {
	if w == nil || w.ResetsAt == nil {
		return "—"
	}
	return formatUntil(*w.ResetsAt)
}

func paint(code, text string, enabled bool) string {
	if !enabled || code == "" {
		return text
	}
	return code + text + colorReset
}

func formatUntil(t time.Time) string {
	d := time.Until(t)
	if d <= 0 {
		return "now"
	}
	days := int(d.Hours()) / 24
	hours := int(d.Hours()) % 24
	minutes := int(d.Minutes()) % 60
	switch {
	case days > 0:
		return fmt.Sprintf("%dd%dh", days, hours)
	case hours > 0:
		return fmt.Sprintf("%dh%02dm", hours, minutes)
	case minutes > 0:
		return fmt.Sprintf("%dm", minutes)
	}
	return "<1m"
}

type resultJSON struct {
	Name      string    `json:"name"`
	Email     string    `json:"email,omitempty"`
	Usage     *usage    `json:"usage,omitempty"`
	Error     string    `json:"error,omitempty"`
	FetchedAt time.Time `json:"fetched_at"`
}

func toJSON(results []result) []resultJSON {
	out := make([]resultJSON, 0, len(results))
	for _, r := range results {
		item := resultJSON{Name: r.Account.Name, Email: r.Account.Email, Usage: r.Usage, FetchedAt: r.FetchedAt}
		if r.Err != nil {
			item.Error = r.Err.Error()
		}
		out = append(out, item)
	}
	return out
}

func writeJSON(w io.Writer, results []result) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(toJSON(results)); err != nil {
		return fmt.Errorf("encode results: %w", err)
	}
	return nil
}
