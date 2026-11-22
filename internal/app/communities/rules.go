package communities

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"strconv"
	"strings"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/standing"
)

const (
	basePath = "/fleets/communities/"

	nameMax        = 60
	descriptionMax = 500
	slugMax        = 40
	ledPerAccount  = 5
	membersPerPage = 50

	// How a community takes new members (communities.join_policy).
	// Stored: never rename.
	policyOpen     = "open"
	policyApproval = "approval"
	policyInvite   = "invite"

	// Where a character stands (community_members.state). Stored:
	// never rename.
	stateActive  = "active"
	statePending = "pending"
	stateBanned  = "banned"

	// What a leader or admin can do to a member.
	doApprove = "approve"
	doDecline = "decline"
	doBan     = "ban"
	doUnban   = "unban"
	doRemove  = "remove"
	doAdmin   = "admin"
	doMember  = "member"
	doLead    = "lead"
)

// policyOption is one way of taking new members, as a form offers it.
type policyOption struct {
	Value, Label, Hint string
	Selected           bool
}

func policyOptions(current string) []policyOption {
	out := []policyOption{
		{Value: policyApproval, Label: "Leaders approve", Hint: "Anyone can ask. A leader or admin says yes or no."},
		{Value: policyOpen, Label: "Anyone can join", Hint: "Pilots join straight away."},
		{Value: policyInvite, Label: "Invite link only", Hint: "Only people with your link can join."},
	}
	for i := range out {
		out[i].Selected = out[i].Value == current
	}
	return out
}

func policyLabel(policy string) string {
	for _, o := range policyOptions(policy) {
		if o.Selected {
			return o.Label
		}
	}
	return policy
}

func validPolicy(policy string) bool {
	return policy == policyOpen || policy == policyApproval || policy == policyInvite
}

// roleLabel is a place in a community, in words.
func roleLabel(role string) string {
	switch role {
	case standing.CommunityLeader:
		return "Leader"
	case standing.CommunityAdmin:
		return "Admin"
	}
	return "Member"
}

// slugFor makes a community's address from its name: lower-case letters
// and digits joined by single dashes.
func slugFor(name string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(name) {
		if b.Len() >= slugMax {
			break
		}
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			dash = false
		} else if b.Len() > 0 && !dash {
			b.WriteByte('-')
			dash = true
		}
	}
	slug := strings.Trim(b.String(), "-")
	if slug == "" || slug == "create" {
		return "community"
	}
	return slug
}

func newInviteToken() (string, error) {
	buf := make([]byte, 18)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// validInvite reports whether a token is the community's current
// invite link.
func validInvite(c db.Community, token string) bool {
	return token != "" && c.InviteToken.Valid &&
		subtle.ConstantTimeCompare([]byte(token), []byte(c.InviteToken.String)) == 1
}

// wantsLink reports whether a community needs a private link: one that
// is not listed, or one that only takes members through its link.
func wantsLink(listed bool, policy string) bool {
	return !listed || policy == policyInvite
}

// visibleTo reports whether someone may see a community at all. A
// listed community is public. Any other is seen only with its private
// link or by a character that already holds a row in it.
func visibleTo(c db.Community, places []db.CommunityMember, token string) bool {
	return c.Listed || len(places) > 0 || validInvite(c, token)
}

// memberAction is one thing a leader or admin can do to a row.
type memberAction struct {
	Do, Label, Confirm string
	Danger             bool
}

// actionsFor lists what someone in the actor's place can do to a
// member. Nobody acts on an equal or higher place. Only a leader
// changes places, and only a character on a working account can be
// made an admin or leader: a pilot with no account never runs anything.
func actionsFor(actor, targetRole, targetState, targetName string, targetHasAccount bool) []memberAction {
	actorRank := standing.CommunityRank(actor)
	if actorRank < standing.CommunityRank(standing.CommunityAdmin) {
		return nil
	}
	ban := memberAction{Do: doBan, Label: "Ban", Confirm: "Ban " + targetName + " from this community?", Danger: true}
	switch targetState {
	case statePending:
		return []memberAction{{Do: doApprove, Label: "Approve"}, {Do: doDecline, Label: "Decline"}, ban}
	case stateBanned:
		return []memberAction{{Do: doUnban, Label: "Lift ban"}}
	case stateActive:
		if standing.CommunityRank(targetRole) >= actorRank {
			return nil
		}
		var out []memberAction
		if actor == standing.CommunityLeader {
			if targetRole == standing.CommunityMember && targetHasAccount {
				out = append(out, memberAction{Do: doAdmin, Label: "Make admin"})
			}
			if targetRole == standing.CommunityAdmin {
				out = append(out, memberAction{Do: doMember, Label: "Remove admin"})
			}
			if targetHasAccount {
				out = append(out, memberAction{Do: doLead, Label: "Make leader", Confirm: "Make " + targetName + " the leader? You become an admin."})
			}
		}
		return append(out, memberAction{Do: doRemove, Label: "Remove", Confirm: "Remove " + targetName + " from this community?"}, ban)
	}
	return nil
}

// countText writes a count with thousands separators.
func countText(n int32) string {
	s := strconv.Itoa(int(n))
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

// memberWord is "member" or "members" for a count.
func memberWord(n int32) string {
	if n == 1 {
		return "member"
	}
	return "members"
}

// likePattern is a search term as an ILIKE contains pattern, its
// wildcards taken literally.
func likePattern(term string) string {
	if term == "" {
		return ""
	}
	return "%" + strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(term) + "%"
}
