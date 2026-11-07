package orders

import (
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
