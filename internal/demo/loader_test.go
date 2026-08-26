package demo

import (
	"context"
	"testing"

	"github.com/vanilla-bar/gh-issue-graph/internal/graph"
)

// The demo dataset is the fixture the UI is developed against, so it has to
// keep covering every visual state. This test pins that coverage.
func TestDemoCoversEveryVisualState(t *testing.T) {
	in, err := New().Load(context.Background(), graph.SearchOptions{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	result := graph.Build(in)

	if len(in.Repositories) != 2 {
		t.Fatalf("repositories = %d, want 2 so the lane separator is exercised", len(in.Repositories))
	}

	kinds := map[string]int{}
	for _, edge := range result.Edges {
		kinds[edge.Kind]++
	}
	for _, kind := range []string{graph.EdgeParent, graph.EdgePRCloses, graph.EdgePRRefs, graph.EdgePRStack, graph.EdgeBlocked, graph.EdgeDuplicate} {
		if kinds[kind] == 0 {
			t.Errorf("demo data draws no %q edge", kind)
		}
	}

	var attention, actionable, blocked, complements, closed int
	relations := map[string]int{}
	for _, issue := range in.Issues {
		relations[issue.Relation]++
		if len(issue.Attention) > 0 {
			attention++
		}
		if issue.Actionable {
			actionable++
		}
		if len(issue.BlockedByIDs) > 0 {
			blocked++
		}
		if issue.Source == "complement" {
			complements++
		}
		if issue.State == "CLOSED" {
			closed++
		}
	}
	for _, relation := range []string{graph.RelationAssigned, graph.RelationMine, graph.RelationOther} {
		if relations[relation] == 0 {
			t.Errorf("demo data has no %q issue", relation)
		}
	}
	if attention < 2 {
		t.Errorf("attention badges = %d, want both reasons represented", attention)
	}
	if actionable == 0 || blocked == 0 || complements == 0 || closed == 0 {
		t.Errorf("actionable=%d blocked=%d complements=%d closed=%d, want all non-zero", actionable, blocked, complements, closed)
	}

	states := map[string]int{}
	for _, pr := range in.PullRequests {
		states[pr.State]++
	}
	if states["OPEN"] == 0 || states["MERGED"] == 0 {
		t.Errorf("pull request states = %v, want open and merged", states)
	}
}

func TestDemoReportsItIsNotTalkingToGitHub(t *testing.T) {
	in, err := New().Load(context.Background(), graph.SearchOptions{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(in.Warnings) == 0 {
		t.Fatal("demo mode should say it is showing canned data")
	}
}
