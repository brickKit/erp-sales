package orders

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestTheLifecycleDecidesEveryCell: every phase decides every command, and each cell is complete for its effect. A
// new phase or command fails here until each of its cells is decided.
func TestTheLifecycleDecidesEveryCell(t *testing.T) {
	require.Len(t, lifecycle, len(Commands), "the table has exactly the commands of Commands")
	stored := map[string]bool{"draft": true, "confirming": true, "confirmed": true, "completed": true, "cancelled": true}
	for _, c := range Commands {
		require.Len(t, lifecycle[c], len(Phases), "command %s decides exactly the phases of Phases", c)
		for _, p := range Phases {
			d, ok := lifecycle[c][p]
			require.True(t, ok, "%s in %s is not decided", c, p)
			assert.Regexp(t, `^SO\d\d$`, d.Rule, "%s in %s names its rule", c, p)
			switch d.Effect {
			case Refuse:
				assert.NotEmpty(t, d.Refusal, "%s in %s: a refusal has its words", c, p)
			case Move:
				assert.True(t, stored[d.To], "%s in %s moves to a stored state, not %q", c, p, d.To)
				assert.Equal(t, d.Audit, d.Op != "", "%s in %s: an audited move names its operation", c, p)
			case Process:
				assert.Contains(t, []string{"confirmation", "cancel"}, d.Via, "%s in %s: a process names its kind", c, p)
			case Ignore, Proceed:
			default:
				t.Errorf("%s in %s: unknown effect %q", c, p, d.Effect)
			}
		}
	}
}

// TestStoredPhaseIsEnough: StoredPhase cannot tell a cancel under way apart, so it decides only commands whose
// Confirmed and Cancelling cells are the same.
func TestStoredPhaseIsEnough(t *testing.T) {
	for _, c := range StoredPhaseCommands {
		assert.Equal(t, Decide(Confirmed, c), Decide(Cancelling, c), "%s cannot be decided on the stored phase", c)
	}
}

// TestTheDesignDocumentIsTheLifecycle: the table in docs/design.md ("订单的状态") is this table, cell by cell (rule,
// effect, state after, process).
func TestTheDesignDocumentIsTheLifecycle(t *testing.T) {
	doc, err := os.ReadFile("../../docs/design.md")
	require.NoError(t, err)
	var phases []Phase
	seen := map[Command]bool{}
	cell := regexp.MustCompile("^(允许|拒绝|忽略|→ `(\\w+)`|经确认过程|经取消过程) (SO\\d\\d)$")
	tick := regexp.MustCompile("`(\\w+)`")
	for _, line := range strings.Split(string(doc), "\n") {
		cells := strings.Split(strings.Trim(line, "| "), " | ")
		if strings.HasPrefix(line, "| 命令 | ") {
			for _, c := range cells[1:] {
				phases = append(phases, Phase(strings.Trim(c, "`")))
			}
			require.Equal(t, Phases, phases, "the document's phases are Phases, in order")
			continue
		}
		if phases == nil || !strings.HasPrefix(line, "| ") || len(cells) != len(phases)+1 {
			if phases != nil && len(seen) > 0 && !strings.HasPrefix(line, "| ") {
				break // the end of the table
			}
			continue
		}
		names := tick.FindAllStringSubmatch(cells[0], -1)
		if len(names) == 0 {
			continue // the separator row
		}
		c := Command(names[len(names)-1][1])
		require.Contains(t, lifecycle, c, "the document has a command the code does not: %s", c)
		seen[c] = true
		for i, p := range phases {
			m := cell.FindStringSubmatch(cells[i+1])
			require.NotNil(t, m, "%s in %s: %q is not a cell", c, p, cells[i+1])
			want := lifecycle[c][p]
			got := Decision{Rule: m[3]}
			switch {
			case m[1] == "允许":
				got.Effect = Proceed
			case m[1] == "拒绝":
				got.Effect = Refuse
			case m[1] == "忽略":
				got.Effect = Ignore
			case m[1] == "经确认过程":
				got.Effect, got.Via = Process, "confirmation"
			case m[1] == "经取消过程":
				got.Effect, got.Via = Process, "cancel"
			default:
				got.Effect, got.To = Move, m[2]
			}
			assert.Equal(t, [4]string{want.Rule, string(want.Effect), want.To, want.Via},
				[4]string{got.Rule, string(got.Effect), got.To, got.Via}, "%s in %s: the document and the code differ", c, p)
		}
	}
	assert.Len(t, seen, len(Commands), "the document decides every command")
}

// TestEveryRuleIsTested: each rule of the lifecycle is named by a test that exercises it (a comment in itest/).
func TestEveryRuleIsTested(t *testing.T) {
	files, err := filepath.Glob("../../itest/*_test.go")
	require.NoError(t, err)
	var tests strings.Builder
	for _, f := range files {
		b, err := os.ReadFile(f)
		require.NoError(t, err)
		tests.Write(b)
	}
	named := map[string]bool{}
	for _, r := range regexp.MustCompile(`\bSO\d\d\b`).FindAllString(tests.String(), -1) {
		named[r] = true
	}
	for _, c := range Commands {
		for _, p := range Phases {
			rule := lifecycle[c][p].Rule
			assert.True(t, named[rule], "no test in itest/ names %s (%s in %s)", rule, c, p)
		}
	}
}
