# Roadmap

Recorded from Nate's roadmap on 2026-10-08. This is direction, not a
commitment: nothing here is scheduled, and each item should land as small
PRs with tests. The v0.5 module manifest work is separate and lands first.

## Notifications (backbone)

The app already pulls and digests the data for the overview page; what is
missing is the layer that tells users. A settings page offers:

1. **In-app only.** A small mail icon next to the character switcher on
   large screens, and a small icon next to search on mobile. Icons are
   type-specific where it helps (unopened letter for mail, a planet with a
   red dot for PI) and fall back to a generic unread icon when there is more
   than one. Clicking shows a summarized list.
2. **Browser notifications**, when the user chooses them and the browser
   permits.
3. **Per-type choice** of which notifications to receive, if any. Applies to
   both modes.

Notification types: watch list alerts, skill complete, new mail, new
calendar event, new killmail, stopped PI, industry job complete. Future:
approved SRP, fleet ops, new fit rating.

Open decision: service worker and PWA scope for background push (see
`docs/decisions/push-and-pwa.md`).

## Corp management (the SeAT replacement)

- **SRP.** Members submit killmails, officers approve or deny, payouts are
  tracked. The wedge: the main reason corps run SeAT.
- **Fleet ops calendar and PAP tracking.** Schedule ops; track attendance
  from ESI fleet history.
- **Recruitment pipeline.** Application forms, ESI background checks
  (skills, killboard, employment history), accept/reject workflow.
- **Mining ledger and tax.** Corp mining ops with per-member contribution.

## Intel

- **Wormhole mapper.** The biggest gap: Tripwire and Pathfinder are aging
  and self-hosted. Chain mapping, signature tracking, mass and critical
  states.
- **D-scan parser.** Paste parsing now (cheap). Combined with character
  data, auto-flag hostiles by standing. Live local/d-scan capture needs
  something running next to the game client, so it comes later as a browser
  extension or companion app.

## Differentiation

- **Doctrine manager.** Ties the fitting tool, public fits and corp
  management together: FCs publish doctrines, members one-click fit from
  corp contracts or market, a compliance checker shows who is flying what.
- **Contract finder.** Search public contracts across regions with real
  filters.
