package routing

import (
	"fmt"
	"strings"
	"time"
)

// selectAffinity reports the candidate a session's pin names, if the pin applies
// at all. It answers "is this the session's route", not "is it viable": the
// caller decides that, because a route excluded for a transient reason is still
// the session's route and switching away from it is the thing this issue forbids.
//
// A pin is refused when it names another session, has no alias, carries no
// expiry (an unbounded pin outlives the route it names), or has expired.
func selectAffinity(cs []candidate, req RouteRequest, state SelectState) (candidate, bool) {
	if state.Affinity == nil {
		return candidate{}, false
	}
	if state.Affinity.SessionID == "" || state.Affinity.SessionID != req.SessionID {
		return candidate{}, false
	}
	if state.Affinity.Alias == "" {
		return candidate{}, false
	}
	if !state.Affinity.ExpiresAt.After(state.Now) {
		return candidate{}, false
	}
	for _, c := range cs {
		if c.Alias == state.Affinity.Alias {
			return c, true
		}
	}
	return candidate{}, false
}

// heldThroughTransient reports whether the route the session is pinned to was
// eliminated only by transient stages. It reads the eliminations already
// recorded, so a stage added later is treated as durable until it says
// otherwise: switching on an unexplained exclusion would leave this decision
// copying the same kind of mistake the transient flag exists to prevent.
func heldThroughTransient(rejected []RejectedCandidate, alias string) (bool, string) {
	var why []string
	for _, r := range rejected {
		if r.Alias != alias {
			continue
		}
		if !r.Transient {
			return false, ""
		}
		why = append(why, r.Reason)
	}
	if len(why) == 0 {
		return false, ""
	}
	return true, strings.Join(why, "; ")
}

// affinityStageNote explains why the pin did not decide this decision, so the
// audit record names the actual reason instead of an ambiguous "no match". A pin
// excluded before it was even resolved — a disabled alias, a capability the
// catalogue no longer claims — is named from its own rejection, because falling
// back to "no viable affinity match" would hide the exclusion an operator needs
// to see.
func affinityStageNote(req RouteRequest, state SelectState, applies bool, rejected []RejectedCandidate) string {
	switch {
	case state.Affinity == nil:
		return "no affinity stored for this session"
	case state.Affinity.SessionID != req.SessionID:
		return "stored affinity belongs to another session"
	case state.Affinity.Alias == "":
		return "stored affinity names no route"
	case !state.Affinity.ExpiresAt.After(state.Now):
		return fmt.Sprintf("affinity for %q expired at %s; re-evaluated from the full candidate set", state.Affinity.Alias, state.Affinity.ExpiresAt.UTC().Format(time.RFC3339))
	}
	// A pin excluded before resolution never becomes an affinity candidate, so
	// its elimination is the only place the reason exists.
	if !applies {
		if r, ok := rejectedCandidate(rejected, state.Affinity.Alias); ok {
			return fmt.Sprintf("affinity route %q excluded at %s: %s; session must move", r.Alias, r.Stage, r.Reason)
		}
		return "no viable affinity match"
	}
	return fmt.Sprintf("affinity route %q eliminated by a durable exclusion; session must move", state.Affinity.Alias)
}

func rejectedCandidate(cs []RejectedCandidate, alias string) (RejectedCandidate, bool) {
	for _, r := range cs {
		if r.Alias == alias {
			return r, true
		}
	}
	return RejectedCandidate{}, false
}

func affinityEliminations(cs []candidate, selected string) []RejectedCandidate {
	var elim []RejectedCandidate
	for _, c := range cs {
		if c.Alias == selected {
			continue
		}
		elim = append(elim, RejectedCandidate{Alias: c.Alias, Provider: c.Provider, Stage: "session_affinity", Reason: "session affinity retained another viable route"})
	}
	return elim
}
