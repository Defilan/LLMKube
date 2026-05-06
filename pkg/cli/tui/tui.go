/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

// Package tui implements the `llmkube tui` interactive model picker, deploy
// configurator, and live status watcher. v0.1 ships the browser view only;
// the deploy form and status view are scaffolded as follow-up work.
package tui

import (
	"fmt"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"
)

// view enumerates which sub-screen the TUI is showing. The root model holds
// one of these and routes Update/View calls to the matching subview.
type view int

const (
	viewBrowser view = iota
	viewDeploy       // reserved for v0.2: deploy form
	viewStatus       // reserved for v0.2: live status watcher
)

// openDeployFormMsg asks the root model to switch from the browser into the
// deploy form, pre-filled from the selected row.
type openDeployFormMsg struct {
	seed seedInput
}

// rootModel composes all subviews and routes input. Bubbletea's tea.Model
// interface (Init/Update/View) is implemented at this top level; subviews
// are plain structs the root delegates to.
type rootModel struct {
	current  view
	browser  browserModel
	deploy   deployFormModel
	width    int
	height   int
	quitting bool
	// kubeErr captures any cluster-connection error so the browser can render
	// a "no cluster" banner without blocking the local-disk view.
	kubeErr error
}

func (m rootModel) Init() tea.Cmd {
	return m.browser.Init()
}

func (m rootModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.browser.width = msg.Width
		m.browser.height = msg.Height
	case tea.KeyMsg:
		// "q" must NOT quit while in the deploy form: typing "q" in a name
		// field is legitimate. ctrl+c always quits.
		if msg.String() == "ctrl+c" {
			m.quitting = true
			return m, tea.Quit
		}
		if msg.String() == "q" && m.current == viewBrowser {
			m.quitting = true
			return m, tea.Quit
		}
	case openDeployFormMsg:
		m.deploy = newDeployForm(msg.seed)
		m.current = viewDeploy
		return m, m.deploy.Init()
	case formCancelMsg:
		m.current = viewBrowser
		return m, nil
	case deployedMsg:
		// v0.2 will switch to viewStatus here. For now, surface success in
		// the form's terminal state so the user knows the apply landed.
		m.deploy.deployedName = msg.name
		m.deploy.deployedNamespace = msg.namespace
		return m, nil
	case deployErrMsg:
		m.deploy.err = msg.err
		m.deploy.applied = false
		return m, nil
	}

	switch m.current {
	case viewBrowser:
		bm, cmd := m.browser.Update(msg)
		m.browser = bm.(browserModel)
		return m, cmd
	case viewDeploy:
		dm, cmd := m.deploy.Update(msg)
		m.deploy = dm.(deployFormModel)
		return m, cmd
	}
	return m, nil
}

func (m rootModel) View() string {
	if m.quitting {
		return ""
	}
	switch m.current {
	case viewBrowser:
		return m.browser.View()
	case viewDeploy:
		return m.deploy.View()
	}
	return ""
}

// NewCommand returns the Cobra subcommand wiring `llmkube tui`. Registered
// from pkg/cli/root.go.
func NewCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "tui",
		Short: "Interactive model picker and deploy configurator",
		Long: `Launch an interactive TUI for browsing local + catalog models,
configuring deploy options, and applying them to your cluster.

v0.1 ships the model browser. Deploy form and live status views are
in-flight; until then, use 'llmkube deploy <model-id>' to apply
selections from the catalog.`,
		RunE: runTUI,
	}
	return cmd
}

func runTUI(cmd *cobra.Command, args []string) error {
	// Best-effort cluster check: a missing kubeconfig should not block the
	// user from browsing local models. The browser displays a banner when
	// kubeErr is set.
	var kubeErr error
	if _, err := newK8sClient(); err != nil {
		kubeErr = err
	}

	browser, err := newBrowser()
	if err != nil {
		return fmt.Errorf("failed to initialize browser: %w", err)
	}

	root := rootModel{
		current: viewBrowser,
		browser: browser,
		kubeErr: kubeErr,
	}
	root.browser.kubeErr = kubeErr

	p := tea.NewProgram(root, tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		return fmt.Errorf("tui exited with error: %w", err)
	}
	return nil
}
