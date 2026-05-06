/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package tui

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/defilantech/llmkube/catalog"
)

// row is a unified browser entry: either a catalog model or a local-disk
// finding. The browser merges both sources into one scrollable list so the
// user sees their working set in one place.
type row struct {
	// Identity
	ID      string // catalog ID (e.g. "phi-4-mini") or display name for local-only
	IsLocal bool   // true when this row originated from a disk scan
	Local   *LocalModel
	Catalog *catalog.Model // pointer into the loaded catalog
	// Display fields precomputed at construction so Update doesn't recompute
	// per keystroke.
	Name        string
	Sub         string // "8B • Q8_0 • 35.2 GB" or similar
	Section     string // "Catalog" or "Detected on disk"
	Downloaded  bool   // catalog row that resolves to a local file
	DownloadHit string // path of the resolved local file, when Downloaded
}

type browserModel struct {
	rows     []row
	filtered []int // indices into rows; respects the search filter
	cursor   int   // index into filtered
	search   textinput.Model
	width    int
	height   int
	kubeErr  error // surfaced as a banner when set
}

func newBrowser() (browserModel, error) {
	cat, err := catalog.Load()
	if err != nil {
		return browserModel{}, fmt.Errorf("load catalog: %w", err)
	}
	local := ScanLocal()

	rows := mergeRows(cat, local)

	ti := textinput.New()
	ti.Placeholder = "Type to filter (esc to clear)..."
	ti.CharLimit = 80

	bm := browserModel{
		rows:   rows,
		search: ti,
	}
	bm.applyFilter()
	return bm, nil
}

// mergeRows combines catalog entries and local-disk findings into the unified
// row list. Catalog entries appear first; each is annotated as Downloaded
// when the source filename matches a local find. Local-only entries (not
// matched to any catalog row) appear in the "Detected on disk" section.
func mergeRows(cat *catalog.Catalog, local []LocalModel) []row {
	// Build an index from local filenames (basename, lowercase) to LocalModel
	// pointers so catalog rows can resolve their downloaded state in O(1).
	localByFile := make(map[string]*LocalModel, len(local))
	for i := range local {
		key := strings.ToLower(filepath.Base(local[i].Path))
		localByFile[key] = &local[i]
	}

	consumed := make(map[string]struct{}, len(local))
	var rows []row

	// Catalog section, sorted by catalog ID for deterministic order.
	catIDs := make([]string, 0, len(cat.Models))
	for id := range cat.Models {
		catIDs = append(catIDs, id)
	}
	sort.Strings(catIDs)

	for _, id := range catIDs {
		entry := cat.Models[id]
		r := row{
			ID:      id,
			Catalog: &entry,
			Name:    fmt.Sprintf("%s — %s", id, entry.Name),
			Sub:     formatCatalogSub(&entry),
			Section: "Catalog",
		}
		// Resolution: catalog Source URL ends in a filename; match the local
		// scan by basename. Avoids false matches when two distinct catalog
		// entries point at the same upstream filename.
		if entry.Source != "" {
			candidate := strings.ToLower(filepath.Base(entry.Source))
			if hit, ok := localByFile[candidate]; ok {
				r.Downloaded = true
				r.DownloadHit = hit.Path
				consumed[hit.Path] = struct{}{}
			}
		}
		rows = append(rows, r)
	}

	// Detected-on-disk section: any local find not consumed by a catalog match.
	for i := range local {
		if _, ok := consumed[local[i].Path]; ok {
			continue
		}
		l := local[i]
		rows = append(rows, row{
			ID:      l.DisplayName,
			IsLocal: true,
			Local:   &l,
			Name:    l.DisplayName,
			Sub:     formatLocalSub(&l),
			Section: "Detected on disk",
		})
	}

	return rows
}

func formatCatalogSub(m *catalog.Model) string {
	parts := []string{m.Size}
	if m.Quantization != "" {
		parts = append(parts, m.Quantization)
	}
	if m.VRAMEstimate != "" {
		parts = append(parts, m.VRAMEstimate)
	}
	return strings.Join(parts, " • ")
}

func formatLocalSub(l *LocalModel) string {
	parts := []string{l.Format}
	if l.Quant != "" {
		parts = append(parts, l.Quant)
	}
	parts = append(parts, humanSize(l.SizeBytes))
	parts = append(parts, fmt.Sprintf("from %s", filepath.Base(l.SourceDir)))
	return strings.Join(parts, " • ")
}

func humanSize(b int64) string {
	const (
		_  = iota
		kb = 1 << (10 * iota)
		mb
		gb
	)
	switch {
	case b >= gb:
		return fmt.Sprintf("%.1f GB", float64(b)/float64(gb))
	case b >= mb:
		return fmt.Sprintf("%.0f MB", float64(b)/float64(mb))
	case b >= kb:
		return fmt.Sprintf("%.0f KB", float64(b)/float64(kb))
	default:
		return fmt.Sprintf("%d B", b)
	}
}

func (m browserModel) Init() tea.Cmd {
	return textinput.Blink
}

func (m browserModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		// When the search input is focused, route most keys to it; otherwise
		// handle as navigation.
		if m.search.Focused() {
			switch msg.String() {
			case "esc":
				m.search.SetValue("")
				m.search.Blur()
				m.applyFilter()
				return m, nil
			case "enter":
				m.search.Blur()
				return m, nil
			}
			var cmd tea.Cmd
			m.search, cmd = m.search.Update(msg)
			m.applyFilter()
			return m, cmd
		}

		switch msg.String() {
		case "/":
			m.search.Focus()
			return m, textinput.Blink
		case "up", "k":
			if m.cursor > 0 {
				m.cursor--
			}
		case "down", "j":
			if m.cursor < len(m.filtered)-1 {
				m.cursor++
			}
		case "g":
			m.cursor = 0
		case "G":
			if len(m.filtered) > 0 {
				m.cursor = len(m.filtered) - 1
			}
		case "enter":
			// Emit a deploy-form message with the selected row's seed.
			// Root model handles the transition.
			if len(m.filtered) == 0 {
				return m, nil
			}
			selected := m.rows[m.filtered[m.cursor]]
			var seed seedInput
			switch {
			case selected.IsLocal:
				seed = seedFromLocal(selected.Local)
			case selected.Catalog != nil:
				seed = seedFromCatalog(selected.ID, selected.Catalog)
			default:
				return m, nil
			}
			return m, func() tea.Msg { return openDeployFormMsg{seed: seed} }
		}
	}
	return m, nil
}

// applyFilter rebuilds m.filtered based on the current search term. Search
// is a case-insensitive substring match against Name + Sub + ID.
func (m *browserModel) applyFilter() {
	term := strings.ToLower(strings.TrimSpace(m.search.Value()))
	m.filtered = m.filtered[:0]
	for i, r := range m.rows {
		if term == "" || strings.Contains(strings.ToLower(r.Name+" "+r.Sub+" "+r.ID), term) {
			m.filtered = append(m.filtered, i)
		}
	}
	if m.cursor >= len(m.filtered) {
		m.cursor = 0
	}
}

func (m browserModel) View() string {
	var sb strings.Builder

	// Header
	sb.WriteString(styleHeader.Render("LLMKube · Model browser"))
	sb.WriteString("\n")
	if m.kubeErr != nil {
		sb.WriteString(styleBadgePending.Render(
			fmt.Sprintf("⚠ no cluster reachable: %s (browse only; deploy will fail)", trimErr(m.kubeErr.Error())),
		))
		sb.WriteString("\n")
	}

	// Search bar (always rendered, blurred when not focused)
	sb.WriteString(m.search.View())
	sb.WriteString("\n")

	// Body: windowed row list. Each row renders as 2 lines (name+badge,
	// then sub). Section headers cost 2 lines (blank separator + label).
	// We compute available row capacity from m.height so the View output
	// fits the terminal even when alt-screen mode is on; otherwise the
	// box overflows and the top scrolls off-screen.
	bodyLines := availableBodyLines(m.height, m.kubeErr != nil)
	maxRows := bodyLines / rowLines
	if maxRows < 1 {
		maxRows = 1
	}

	if len(m.filtered) == 0 {
		sb.WriteString(styleDim.Render("  (no models match)"))
	} else {
		windowStart, windowEnd := computeWindow(m.cursor, len(m.filtered), maxRows)

		// Scroll indicator: rows hidden above the window.
		if windowStart > 0 {
			sb.WriteString(styleDim.Render(fmt.Sprintf("  ↑ %d more above", windowStart)))
			sb.WriteString("\n")
		}

		var lastSection string
		for visIdx := windowStart; visIdx < windowEnd; visIdx++ {
			rowIdx := m.filtered[visIdx]
			r := m.rows[rowIdx]
			if r.Section != lastSection {
				sb.WriteString(styleSection.Render(r.Section))
				sb.WriteString("\n")
				lastSection = r.Section
			}
			sb.WriteString(renderRow(r, visIdx == m.cursor))
			sb.WriteString("\n")
		}

		// Scroll indicator: rows hidden below the window.
		if windowEnd < len(m.filtered) {
			sb.WriteString(styleDim.Render(fmt.Sprintf("  ↓ %d more below", len(m.filtered)-windowEnd)))
			sb.WriteString("\n")
		}
	}

	// Help footer (anchored at the bottom of the rendered block)
	sb.WriteString(styleHelp.Render(
		"↑/↓ navigate · / search · g/G top/bottom · enter (deploy form coming in v0.2) · q quit",
	))

	width := m.width - 2
	if width > 120 {
		width = 120
	}
	if width < 40 {
		width = 40
	}
	return styleBox.Width(width).Render(sb.String())
}

// rowLines is how many vertical lines one row takes when rendered. Two:
// the name + badge line, and the dim sub-info line.
const rowLines = 2

// chromeOverhead is the number of lines the static UI chrome consumes:
// header (1), blank (1), search bar (1), blank (1), footer (1), and box
// borders + padding (2). Tuned empirically to fit on an 80x24 terminal.
const chromeOverhead = 7

// availableBodyLines returns how many lines remain for the row list after
// reserving the header/footer chrome and (optionally) the no-cluster banner.
func availableBodyLines(height int, hasKubeBanner bool) int {
	overhead := chromeOverhead
	if hasKubeBanner {
		overhead++
	}
	avail := height - overhead
	if avail < rowLines {
		return rowLines
	}
	return avail
}

// computeWindow returns [start, end) indices into the filtered row list such
// that the cursor is always inside the visible window. Slides the window
// down when the cursor moves below the bottom edge, up when it moves above
// the top edge.
func computeWindow(cursor, total, capacity int) (int, int) {
	if capacity >= total {
		return 0, total
	}
	// Center the cursor in the window when possible. This keeps "where am I"
	// visible without jarring jumps. When the cursor is near the start or
	// end, the window clamps to the boundary instead of going negative or
	// past the end.
	start := cursor - capacity/2
	if start < 0 {
		start = 0
	}
	end := start + capacity
	if end > total {
		end = total
		start = end - capacity
		if start < 0 {
			start = 0
		}
	}
	return start, end
}

func renderRow(r row, selected bool) string {
	cursor := "  "
	if selected {
		cursor = "▶ "
	}
	badge := ""
	if r.Downloaded {
		badge = styleBadgeReady.Render(" ✓ downloaded")
	} else if r.IsLocal {
		badge = styleBadgePending.Render(" • local-only")
	}
	line := fmt.Sprintf("%s%s%s\n     %s", cursor, r.Name, badge, styleDim.Render(r.Sub))
	if selected {
		return styleSelected.Render(line)
	}
	return line
}

func trimErr(s string) string {
	if len(s) > 80 {
		return s[:77] + "..."
	}
	return s
}
