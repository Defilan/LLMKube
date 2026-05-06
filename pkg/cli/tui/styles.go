/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package tui

import "github.com/charmbracelet/lipgloss"

// Theme palette. Picked to read on both dark and light terminals; lipgloss
// adapts to terminal background detection at runtime so we don't need
// separate definitions per theme.
var (
	colorPrimary = lipgloss.AdaptiveColor{Light: "#1F6FEB", Dark: "#58A6FF"}
	colorMuted   = lipgloss.AdaptiveColor{Light: "#6E7781", Dark: "#8B949E"}
	colorAccent  = lipgloss.AdaptiveColor{Light: "#1A7F37", Dark: "#3FB950"}
	colorWarn    = lipgloss.AdaptiveColor{Light: "#9A6700", Dark: "#D29922"}
	colorBorder  = lipgloss.AdaptiveColor{Light: "#D0D7DE", Dark: "#30363D"}
)

// styleHeader is the title bar at the top of every view.
var styleHeader = lipgloss.NewStyle().
	Bold(true).
	Foreground(colorPrimary).
	Padding(0, 1)

// styleHelp is the dim hint line at the bottom.
var styleHelp = lipgloss.NewStyle().
	Foreground(colorMuted).
	Padding(0, 1)

// styleSection labels a logical section in the browser (e.g. "Catalog",
// "Detected on disk").
var styleSection = lipgloss.NewStyle().
	Bold(true).
	Foreground(colorAccent).
	MarginTop(1)

// styleSelected highlights the currently focused row.
var styleSelected = lipgloss.NewStyle().
	Bold(true).
	Foreground(colorPrimary).
	Background(lipgloss.AdaptiveColor{Light: "#DDF4FF", Dark: "#161B22"})

// styleDim is for secondary text on a row (size, quant, path).
var styleDim = lipgloss.NewStyle().
	Foreground(colorMuted)

// styleBadge marks downloaded / detected / will-download status.
var styleBadgeReady = lipgloss.NewStyle().
	Foreground(colorAccent).
	Bold(true)

var styleBadgePending = lipgloss.NewStyle().
	Foreground(colorWarn)

// styleBox wraps the main content in a soft border so the TUI feels framed
// in any terminal.
var styleBox = lipgloss.NewStyle().
	Border(lipgloss.RoundedBorder()).
	BorderForeground(colorBorder).
	Padding(0, 1)
