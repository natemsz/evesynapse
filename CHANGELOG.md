# Changelog

All notable changes to EveSynapse are documented here.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [v0.8.0.0] - 2026-11-09

Automated build of EveSynapse v0.8.0.0. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).

## What's Changed

### Other Changes
* Block botnet traffic using protocol and client hint behavioral fingerprinting by @natemsz in #265

## [v0.7.9.8] - 2026-11-08

Automated build of EveSynapse v0.7.9.8. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).

## What's Changed

### 🚀 Features
* Materialize entity kill involvement lookups to prevent incremental sort planner trap by @natemsz in #264

## [v0.7.9.7] - 2026-11-08

Automated build of EveSynapse v0.7.9.7. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).

## What's Changed

### 🚀 Features
* Add roll sliders with real ranges and meta marks to abyssal search by @natemsz in #263

### Other Changes
* feat/dominion-v3-default-theme by @natemsz in #262

## [v0.7.9.6] - 2026-11-08

Automated build of EveSynapse v0.7.9.6. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).

## What's Changed

### Other Changes
* Optimize ListBlueprintCopyMedianCandidates query plan by @natemsz in #261

## [v0.7.9.5] - 2026-11-08

Automated build of EveSynapse v0.7.9.5. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).

## What's Changed

### Other Changes
* Refine Minmatar v3 borders and panel tones by @natemsz in #260

## [v0.7.9.4] - 2026-11-08

Automated build of EveSynapse v0.7.9.4. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).

## What's Changed

### 🚀 Features
* Fix market tests for net-of-fees margins and sessionless contexts by @miko in #258
* Use klauspost/compress for gzip by @miko in #256
* Add v3 Minmatar theme, remove demo briefing cards, and make notification cards swipeable by @miko in #255
* Show market spreads net of broker fees and sales tax by @miko in #254
* Retry the AWS CLI download in the release mirror job by @miko in #253

### Other Changes
* Pin market scanner tests and harden sqlc freshness gate by @natemsz in #259
* Block automated AI scrapers and update robots.txt by @natemsz in #257

## [v0.7.9.3] - 2026-11-07

Automated build of EveSynapse v0.7.9.3. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).

## What's Changed

### 🚀 Features
* Fix recent kills showing pricing and link kill items to the market by @miko in #252
* Read table data at --fs-sm, keeping pills and loadout chips at their size by @miko in #236

## [v0.7.9.2] - 2026-11-07

Automated build of EveSynapse v0.7.9.2. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).

## What's Changed

### 🚀 Features
* Smooth the market history chart and add area fill and 7-day average by @miko in #250
* Hold shared core surfaces and recent kills in memory and serve from RAM by @natemsz in #249

### Other Changes
* Document LOW_RAM_MODE in README and .env.example by @natemsz in #251

## [v0.7.9.1] - 2026-11-07

Automated build of EveSynapse v0.7.9.1. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).

## What's Changed

### 🚀 Features
* Hold market stat pack in memory and serve from RAM by @natemsz in #248
* Read killboard listings and stats from zKillboard live instead of the local involvement table by @miko in #245
* Build the search index at worker startup and keep it resident by @miko in #244

### Other Changes
* Use the partial entity indexes on kill reads and index kill ingest time by @natemsz in #247

## [v0.7.9.0] - 2026-11-07

Automated build of EveSynapse v0.7.9.0. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).

## What's Changed

### 🚀 Features
* Backfill kill history only for registered users by @miko in #241
* Keep full kill details for one year, then metadata only by @miko in #240

### Other Changes
* Replace adjusted price card on market item page with house appraisal value by @natemsz in #243
* Use native .l-seg security tabs and right-hand watchlist inspector on market item page by @natemsz in #242

## [v0.7.8.9] - 2026-11-07

Automated build of EveSynapse v0.7.8.9. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).

## What's Changed

### 🚀 Features
* Add LOW_RAM_MODE flag and document every env var in .env.example by @miko in #237

### Other Changes
* Tune scheme text neutrals, secondary highlights, and panel surface contrast by @natemsz in #239
* Lay out the market item details page in a responsive 3-tier grid with order book filtering by @natemsz in #238

## [v0.7.8.8] - 2026-11-07

Automated build of EveSynapse v0.7.8.8. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).

## What's Changed

### 🚀 Features
* Add the owner's screenshots to the landing page gallery by @miko in #235
* Store demo pilot records in the envelope the pilot page reads and seed pilot activity profiles by @miko in #234
* List the shipped features the landing page was missing by @miko in #233
* Let long API strings wrap inside the settings card by @miko in #232
* Show the entity's record in the killmail page tiles, with an all-time and 30-day scope swap by @miko in #231
* Add a prepared runner image with the release tools preinstalled by @miko in #230

## [v0.7.8.7] - 2026-11-06

Automated build of EveSynapse v0.7.8.7. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).

## What's Changed

### 🚀 Features
* Walk pre-permanence pilot profiles once more to restore their pruned involvement by @miko in #229
* Show profile-backed pilots their all-time solo kills and ISK destroyed by @miko in #228

## [v0.7.8.6] - 2026-11-06

Automated build of EveSynapse v0.7.8.6. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).

## What's Changed

### 🚀 Features
* Keep priced kill values credited permanently through a slim involvement record by @miko in #227
* Park the fitting module search below the sub-nav when it sticks by @miko in #226

## [v0.7.8.5] - 2026-11-06

Automated build of EveSynapse v0.7.8.5. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).

## What's Changed

### 🚀 Features
* Page the killmail pages past their first 50 rows by @miko in #225
* Store kills past the retention cutoff in post-prune shape at ingest by @miko in #224
* Lay out icon suggestion rows with a gap and text centered on the icon by @miko in #223

## [v0.7.8.4] - 2026-11-06

Automated build of EveSynapse v0.7.8.4. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).

## What's Changed

### 🚀 Features
* Show scoped stats on character, corporation and alliance killboards by @miko in #222
* Resolve killboard entity names when a board or kill page is viewed by @miko in #221
* Set the chip and chip on font size to the 2xs step by @miko in #220
* Remove two stray closing braces from style.css by @miko in #219

## [v0.7.8.2] - 2026-11-06

Automated build of EveSynapse v0.7.8.2. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).

## What's Changed

### 🚀 Features
* Serve search from an in-memory index by @miko in #218
* Add a name search to the killboard by @miko in #217
* Backfill pilot and corporation kill history from zKillboard into the kills store by @miko in #216
* Replace the industry glyph with a mallet and anvil by @miko in #215
* Draw the rail nav icons from the shared glyph sprite by @miko in #214

## [v0.7.8.1] - 2026-11-06

Automated build of EveSynapse v0.7.8.1. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).

## What's Changed

### 🚀 Features
* Gives admin page notes the prose treatment and moves the admin pages into the prose sweep by @miko in #213
* Shows the victim and the final blow pilot with their portraits and organisations in killboard rows by @miko in #212
* Adds a killboard link to corporation and alliance profiles by @miko in #211
* Always shows the item quantity in the Assets inspector by @miko in #209
* Replaces the map, intel, install and sign out nav glyphs with cleaner drawings by @miko in #208
* Sets pill tabs to the 2xs type size by @miko in #207
* Marks the admin pages intro and explainer lines as prose by @miko in #206

## [v0.7.7.7] - 2026-11-06

Automated build of EveSynapse v0.7.7.7. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).

## What's Changed

### 🚀 Features
* Gives the Ship Workbench a state-aware head: doors only until a ship is picked, fit details folded behind the toggle at every width by @miko in #205
* Marks explanatory blocks as prose across the site and sets open prose in Rajdhani Medium by @miko in #204
* Loads admin.css on the ESI Details tab, grids its per-family section, and moves the Admin tabs to the shared sub-nav bar by @miko in #203
* Give prose a readable treatment and mark it on the Appraisal, Intel Channels and Swarm Cache pages by @miko in #202

## [v0.7.7.6] - 2026-11-05

Automated build of EveSynapse v0.7.7.6. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).

## What's Changed

### 🚀 Features
* Batches the swarm history apply onto the existing unnest upsert by @miko in #201

## [v0.7.7.5] - 2026-11-05

Automated build of EveSynapse v0.7.7.5. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).

## What's Changed

### 🚀 Features
* Applies the demo seed daily price drift to holdings instead of net worth by @miko in #200
* Swaps the hot decode sites in internal/app to goccy/go-json by @miko in #199
* Swaps the ESI client's JSON codec to goccy/go-json by @miko in #198
* Adds the Admin ESI Details tab with the right-now strip, worker backlog tiles, and the per-family ESI section by @miko in #197
* Reads CCP's per-route health from /meta/status into the ESI status verdicts by @miko in #196
* Pins the ESI compatibility date on every client request by @miko in #195

## [v0.7.7.4] - 2026-11-05

Automated build of EveSynapse v0.7.7.4. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).

## What's Changed

### 🚀 Features
* Refreshes the Tranquility status snapshot on its own minute ticker, off the worker cycle by @miko in #194
* Add battle reports to the demo account seed by @miko in #193
* Adds EVE Online status and service health links to the data freshness modal by @miko in #192

## [v0.7.7.3] - 2026-11-05

Automated build of EveSynapse v0.7.7.3. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).

## What's Changed

### 🚀 Features
* Fills the demo account with realistic data across pages by @natemsz in #191

## [v0.7.7.2] - 2026-11-05

Automated build of EveSynapse v0.7.7.2. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).

## What's Changed

### 🐛 Bug Fixes
* Moves card, form, KPI and picker layout fixes into the shared layer by @natemsz in #190

## [v0.7.7.1] - 2026-11-05

Automated build of EveSynapse v0.7.7.1. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).

## What's Changed

### 🚀 Features
* Slims the README to the project description, the AI disclosure, and the internals by @miko in #188
* Breaks the Swarm Cache trust section into paragraphs by @miko in #187

### 🐛 Bug Fixes
* Fixes My Loadouts source filters and layout on phones and narrow widths by @natemsz in #189

## [v0.7.7.0] - 2026-11-05

Automated build of EveSynapse v0.7.7.0. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).

## What's Changed

### 🚀 Features
* Rebuilds My Orders and My Contracts from the new mockups and adds Corporation Contracts by @natemsz in #184
* Reads battle reports from game logs: upload, paste and Axon by @natemsz in #183

### 🐛 Bug Fixes
* Keeps the test harness and embedded Postgres out of the release binary by @natemsz in #185
* Closes the tab bypass in redirect checks and drops dead code by @natemsz in #182
* Folds the sub-nav's tabs into More after a page swap by @natemsz in #181

### Other Changes
* Maps the app's layers, modules and their dependencies in the README by @natemsz in #186

## [v0.7.6.3] - 2026-11-05

Automated build of EveSynapse v0.7.6.3. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).

## What's Changed

### 🐛 Bug Fixes
* Completes region sweeps at the end of a shrinking book by @natemsz in #180

## [v0.7.6.2] - 2026-11-05

Automated build of EveSynapse v0.7.6.2. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).

## What's Changed

### 🚀 Features
* Adds battle reports from Axon's game log fights by @natemsz in #179

## [v0.7.6.1] - 2026-11-04

Automated build of EveSynapse v0.7.6.1. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).

## What's Changed

### 🚀 Features
* Rebuilds the phone menu as drill-down screens and polishes phone layouts by @natemsz in #178

### 🐛 Bug Fixes
* Reads bulk ESI pages over parallel connections, backs off failed region sweeps and shrinks the page window after a failure by @natemsz in #176

## [v0.7.6.0] - 2026-11-04

Automated build of EveSynapse v0.7.6.0. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).

## What's Changed

### 🚀 Features
* Stores basket figures where basket values are written by @natemsz in #175
* Marks which blueprints can be built now, for one character or the whole account by @natemsz in #174
* Stores each item's 30-day price line at sweep time and shows it as a hover on listed prices by @natemsz in #173
* Restores legible type sizes, sets UI labels in Rajdhani and puts consoles on the panel glass by @natemsz in #171
* Moves the Map onto the layout framework and removes the overflow clip by @natemsz in #170
* Moves the Tools category onto the layout framework by @natemsz in #167

## [v0.7.5.1] - 2026-11-04

Automated build of EveSynapse v0.7.5.1. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).

## What's Changed

### 🚀 Features
* Re-stamps the swept font sizes onto the current type scale by @natemsz in #169
* Adds a Repository section naming the main home and the mirrors by @miko in #168

## [v0.7.5.0] - 2026-11-04

Automated build of EveSynapse v0.7.5.0. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).

## What's Changed

### 🚀 Features
* Moves the Fitting category onto the layout framework by @natemsz in #166
* Moves the Intel category onto the layout framework with a design pass and one reading face by @natemsz in #165
* Raises the page container's opacity so consoles read as part of the page by @natemsz in #164
* Aligns the Item details button right on the market item page by @natemsz in #163
* Holds the briefing decks to one row with an animated pager by @natemsz in #162
* Builds the Spread Scanner from its new mockups by @natemsz in #161
* Moves the Corporation category onto the layout framework, mirroring the Pilot pages by @natemsz in #160
* Sizes inspector amounts relative to their tile and extends the type-scale sweep to the framework sheets by @natemsz in #159
* Brightens the Specialized specs tabs on Contracts by @natemsz in #158
* Raises the small end of the type scale by @natemsz in #157

## [v0.7.4.0] - 2026-11-04

Automated build of EveSynapse v0.7.4.0. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).

## What's Changed

### 🚀 Features
* Sets the footer in Eve Sans Neue and names Fenris Creations in the legal line by @miko in #156
* Closes the loose ends: bounded Blueprints read, small-account briefing collapse, wallet ranges, and the type scale by @natemsz in #155
* Moves the Market category onto the layout framework, with LP Store, Trends and Baskets built from their mockups by @natemsz in #154
* Builds PI Planner from its mockup and moves the Build Planner onto the framework, closing out Industry by @natemsz in #153
* Adds Copy for EVE and the finish time to Skill Plans, and closes out the Pilots category by @natemsz in #152
* Builds Wallet from its mockup by @natemsz in #151
* Builds Killmails from its mockup and moves the kill page onto the framework by @natemsz in #150

## [v0.7.3.22] - 2026-11-04

Automated build of EveSynapse v0.7.3.22. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).

## What's Changed

### 🚀 Features
* Builds Blueprints from its mockup: product-group directory, library tiles, kind filters, and a blueprint inspector by @natemsz in #148
* Builds Contacts' mockup additions by @natemsz in #147
* Builds Calendar's mockup additions and moves the op pages onto the framework by @natemsz in #146
* Builds Mail's mockup additions and moves Compose onto the framework by @natemsz in #145
* Rebuilds My Colonies from its mockup and brings Mining Ledger to main by @natemsz in #144
* Rebuilds Mining Ledger from its mockup on the layout framework by @natemsz in #142
* Rebuilds My Jobs from its mockup on the layout framework by @natemsz in #141

### Other Changes
* Anchors the demo seed idempotency check to one captured time by @miko in #149
* Replaces the Saira Semi Condensed UI face with Eve Sans Neue by @miko in #143
* Moves the sub-nav bar into the page shell, spans it the full width beside the sidebar, and holds it in place during swaps by @miko in #140

## [v0.7.3.21] - 2026-11-04

Automated build of EveSynapse v0.7.3.21. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).

## What's Changed

### 🚀 Features
* Moves Blueprints onto the layout framework by @natemsz in #139
* Moves Contacts onto the layout framework by @natemsz in #138
* Moves Calendar onto the layout framework with the month beside its events by @natemsz in #137
* Moves Mail onto the layout framework as mailboxes, list and reader by @natemsz in #136
* Moves Wallet onto the layout framework with the ledgers behind a segmented switch by @natemsz in #134
* Moves Killmails onto the layout framework and lets a column title table cards by @natemsz in #132
* Moves Skill Plans onto the layout framework and adds the training timeline by @natemsz in #131
* Adds the layout framework and moves Assets and the Character overview onto it by @natemsz in #130

### 🐛 Bug Fixes
* Rewrites em dashes out of prose copy and fixes the extractor count verb by @natemsz in #129

### Other Changes
* Reads Killmails values from the killboard stored prices by @miko in #135
* Prices kills only with EveSynapse's own appraisal by @miko in #133

## [v0.7.3.19] - 2026-11-03

Automated build of EveSynapse v0.7.3.19. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).

## What's Changed

### Other Changes
* Provisions the database a dev .env names and unexpires the postgres account by @miko in #128

## [v0.7.3.18] - 2026-11-03

Automated build of EveSynapse v0.7.3.18. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).

## What's Changed

### Other Changes
* Takes the home widget tables off the shared scroll-table treatment: no pinned first column and no filled headers by @miko in #126
* Gives setup.sh --build and --build-dev modes, ships it on the releases of all three forges, and points the install docs at it by @miko in #125
* Adds the contribution rules to the AI use disclosure by @miko in #124
* Refreshes the README for the current app by @miko in #119

## [v0.7.3.17] - 2026-11-03

Automated build of EveSynapse v0.7.3.17. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).

## What's Changed

### Other Changes
* Lets long skill names truncate in their cell so the name, the mastered tag and the level pips stop overlapping by @miko in #123
* Paints the pinned first column of scrollable tables so columns scrolling under it stop showing through by @miko in #122

## [v0.7.3.16] - 2026-11-03

Automated build of EveSynapse v0.7.3.16. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).

## What's Changed

### Other Changes
* Keeps the console panes usable in the tablet band and keeps the assets view in place when a ship or stack opens by @miko in #121

## [v0.7.3.15] - 2026-11-03

Automated build of EveSynapse v0.7.3.15. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).

## What's Changed

### Other Changes
* Lands character surface switches at the top of the sheet window by @miko in #120
* Keeps Assets selections in place and routes matrix row clicks through the name link by @miko in #118

## [v0.7.3.14] - 2026-11-03

Automated build of EveSynapse v0.7.3.14. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).

## What's Changed

### 🐛 Bug Fixes
* Wires Assets matrix row clicks on load and on content swaps by @natemsz in #117
* Keeps the Customize resize preview faithful to the saved home by @natemsz in #116

### Other Changes
* Unifies the skill level squares, lands Assets place switches at the top, caps the station directory by @miko in #115
* Puts the briefing first in the default home layout by @miko in #114

## [v0.7.3.13] - 2026-11-03

Automated build of EveSynapse v0.7.3.13. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).

## What's Changed

### 🚀 Features
* Serves per-playstyle default home layouts by @natemsz in #113

## [v0.7.3.12] - 2026-11-03

Automated build of EveSynapse v0.7.3.12. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).

## What's Changed

### Other Changes
* On phones, Assets and the character sheet show their directory first and the picked place or skill group alone by @miko in #112

## [v0.7.3.11] - 2026-11-02

Automated build of EveSynapse v0.7.3.11. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).

## What's Changed

### Other Changes
* Home resize: the grip frame starts over the module it resizes by @miko in #111

## [v0.7.3.10] - 2026-11-02

Automated build of EveSynapse v0.7.3.10. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).

## What's Changed

### Other Changes
* Gives three column pages a shared console class by @miko in #110

## [v0.7.3.9] - 2026-11-02

Automated build of EveSynapse v0.7.3.9. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).

## What's Changed

### Other Changes
* Pads the character console inside its border by @miko in #109

## [v0.7.3.8] - 2026-11-02

Automated build of EveSynapse v0.7.3.8. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).

## What's Changed

### Other Changes
* Gives the character sheet console the frosted glass pane the Assets console uses by @miko in #108

## [v0.7.3.7] - 2026-11-02

Automated build of EveSynapse v0.7.3.7. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).

## What's Changed

### Other Changes
* Moves the gate camp watched systems behind a button and modal, trims the undercut digest row to its frequency dropdown, and stacks the notification rows cleanly on phones by @miko in #107

## [v0.7.3.6] - 2026-11-02

Automated build of EveSynapse v0.7.3.6. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).

## What's Changed

### Other Changes
* Pins the mid-view inspector sheets to the screen, gives the Assets matrix readable columns, and makes the character scope selector apply live by @miko in #106

## [v0.7.3.5] - 2026-11-02

Automated build of EveSynapse v0.7.3.5. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).

## What's Changed

### Other Changes
* Polishes the character sheet and Assets headers, spacing, and controls by @miko in #105

## [v0.7.3.4] - 2026-11-02

Automated build of EveSynapse v0.7.3.4. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).

## What's Changed

### Other Changes
* Keeps the table fill on the header row only by @miko in #104
* Removes the hard offset text shadows by @miko in #103

## [v0.7.3.3] - 2026-11-02

Automated build of EveSynapse v0.7.3.3. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).

## What's Changed

### Other Changes
* Paints the remaining surfaces with the shared theme fills, drops the redundant page wrappers and lets muted scheme glass read through by @miko in #101

## [v0.7.3.2] - 2026-11-02

Automated build of EveSynapse v0.7.3.2. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).

## What's Changed

### Other Changes
* Size home modules inside their floor and ceiling, and backfill the grid by @natemsz in #100

## [v0.7.3.1] - 2026-11-02

Automated build of EveSynapse v0.7.3.1. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).

## What's Changed

### Other Changes
* Fixes broken item icons for skins and blueprints by @miko in #99
* Compact Assets entries and remove singleton and location flag displays by @miko in #98
* Sub-category tabs take the category tab font; Assets head sits tighter to its console by @miko in #97
* Keeps the character scope panel open while the selection is edited by @miko in #96
* Sizes the trained banner to its content by @miko in #95
* Gives the Assets console shell and inspector sheet the site established glass fill by @miko in #94
* Paints the Overview cards, strips and inspector sheet with the site established fills by @miko in #93
* Assets: two-pane console with an overlay inspector at middle widths by @miko in #92
* Gives the Pilot Overview a middle-width layout by @miko in #91
* Links the footer name to the Forgejo organization and the version to its release, drops the commit segment, and prints the binary hash in full by @miko in #90
* Pins blueprint category labels during horizontal table scroll by @miko in #89
* Centers filter chip labels vertically in their pills by @miko in #88
* Adds the Intel watch home widget and the watched systems modal by @miko in #84
* Pins blueprints as production lines and adds the Production Lines home widget by @miko in #83
* Gives every notification kind its own notification and briefing card switches by @natemsz in #81
* Adds a Competition column to My Orders from station heat by @miko in #76
* Links take the theme's secondary tone, one shade lighter on highlight and on the card titles by @miko in #74
* Update https://code.forgejo.org/forgejo/upload-artifact action to v5 by @renovate in #69

## [v0.7.3.0] - 2026-11-02

Automated build of EveSynapse v0.7.3.0. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).

## What's Changed

### Other Changes
* Redesigns the Pilot Overview as a three-pane workspace by @miko in #87
* Shows the running build's commit and binary hash in the footer by @miko in #86
* Rebuilds the Assets page as a three-pane matrix by @miko in #85
* Add the Profit pulse home widget by @miko in #82
* Marks beaten orders inline in the Open orders table by @miko in #79
* Adds a sell list to the appraisal tool, priced to undercut the cheapest sale order by @miko in #78
* Re-anchors the fit tooltip onto the swapped workbench's real slot positions by @miko in #77
* The updater check reads out the mirror with the forges by @miko in #59
* Opens the rail Settings menu upward and gives the phone sheet the settings sections by @miko in #56
* Filters long tables live as you type by @miko in #55

## [v0.7.2.2] - 2026-11-01

Automated build of EveSynapse v0.7.2.2. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).

## What's Changed

### Other Changes
* Installs Node in the Android workflow so its actions can run by @miko in #73
* Migrate Renovate config by @renovate in #72
* Update module github.com/fergusstrange/embedded-postgres to v2 by @renovate in #70
* Update https://code.forgejo.org/forgejo/download-artifact action to v7 by @renovate in #68
* Update Gradle to v9 by @renovate in #67
* Update Go modules by @renovate in #62

## [v0.7.2.1] - 2026-11-01

Automated build of EveSynapse v0.7.2.1. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).

## What's Changed

### Other Changes
* Titles the waterfall in plain English by @miko in #54
* Paints the waterfall gross bar in the theme ink, clear of the recycled slate by @miko in #53
* Paints pinned first table cells with the solid panel by @miko in #52
* Mutes the home Industry and Market widget empty states and links each to its next step by @miko in #51
* Shows the Not explained breakdown as a tooltip instead of an expander by @miko in #50
* Scopes the profit plot height to plot SVGs so figure info tips keep their size by @miko in #49
* Keeps the reader's place when a link only changes the page's own query by @miko in #48
* Draws the info glyph as a filled chip with the italic i in negative space by @miko in #47
* Pins a chart tooltip on a plain tap so tapping off the chart dismisses it by @miko in #46
* Replaces the swap crossfade with a live height glide and staggered draw-in by @miko in #45

## [v0.7.2.0] - 2026-10-31

Automated build of EveSynapse v0.7.2.0. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).

## What's Changed

### Other Changes
* Rebuilds the Profit page around realized profit, capital flow and coverage by @miko in #44

## [v0.7.1.2] - 2026-10-31

Automated build of EveSynapse v0.7.1.2. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).

## What's Changed

### Other Changes
* Labels the Profit charts, adds a day and week breakdown, and rebuilds the demo profit data by @miko in #43
* Puts the briefing dismiss control in the card head beside the alert count by @miko in #42
* Sizes briefing decks by the decks that fit a row at every module width by @miko in #41
* Adds the category header to rail flyouts and keeps group rules from collapsing by @miko in #40
* Pools the theme accent as a glow at the base of the KPI and briefing cards by @miko in #39

## [v0.7.1.1] - 2026-10-31

Automated build of EveSynapse v0.7.1.1. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).

## What's Changed

### Other Changes
* Time-boxes contract appraisal and keeps it off long region sweeps by @miko in #38
* Hides the briefing module while nothing applies by @miko in #37
* Raises the dark theme danger tone to a readable contrast by @miko in #36
* Names each rail flyout with its category and keeps one overlay at a time by @miko in #35
* Moves the LP requirement total into the item tooltip and marks quantities with an x by @miko in #33
* Draws a stepped guide line along the market category tree by @miko in #32
* Keeps Market as one sidebar section with grouped subcategories and group icons by @miko in #31
* Pools the theme accent as a glow at the base of the KPI and briefing cards by @miko in #30
* Renders ISK figures in the house money format on the home KPIs and fleet table by @miko in #29
* Publishes the GitHub release from GitHub Actions instead of the Forgejo pipeline by @miko in #28

## [v0.7.1.0] - 2026-10-31

Automated build of EveSynapse v0.7.1.0. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).

## What's Changed

### Other Changes
* Reorganizes the market navigation, fixes the demo profit charts and the LP store table by @miko in #27

## [v0.7.0.8] - 2026-10-30

Automated build of EveSynapse v0.7.0.8. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).

## What's Changed
### 🚀 Features
* Profit charts: a running total by character, fees with their share of the take, and a cost of goods sold chart
* The updater reads the project's own Forgejo first: with no source named it walks Forgejo, GitLab and GitHub and takes the newest genuine release, with the CDN mirror as the last resort, and EVESYNAPSE_UPDATE_PROVIDER can name one or more sources to walk in order
### Other Changes
* Your-orders pages show only the page's item, or a basket's items on basket pages
* The landing page's section headlines carry the ember gradient again
* The project's code, CI and releases live on its own Forgejo at natems.dev; signed releases publish there and mirror to the CDN

## [v0.7.0.5] - 2026-10-29

Automated build of EveSynapse v0.7.0.5. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).


## What's Changed
### 🚀 Features
* Add market groups to the basket editor search with bulk add
* Replace the profit owner pills with the searchable character picker
* Profit fallbacks: LP drops that mixed two offers are solved by the LP store payment, a job's output can be tied to an open sell order, and the blueprint page shows the item lost on killmails
* Count invention and contract sales in profit per blueprint: the SDE import keeps invention rows, inventions consume their datacores and charge their T2 blueprint's runs, and completed item exchange contracts take their items out of stock at their share of the price
### Other Changes
* Profit page: charts two to a row on wide screens, the site's chart tip and axis labels on every chart, and an income donut beside the activity table
* Fold briefing decks at the midpoint with one painted card per deck
* Market tree: a type ladder, a working indent and a branches-first order
* Set dropdown menu items in Rajdhani Regular at 0.03em letter spacing
* Check for SDE updates daily instead of weekly

## [v0.7.0.4] - 2026-10-29

Automated build of EveSynapse v0.7.0.4. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).


## What's Changed
### 🐛 Bug Fixes
* Drop the stale fresh-assertion flag from the API key creation form by @natemsz in https://github.com/natemsz/evesynapse/pull/821
### Other Changes
* Profit charts polish: LP income in the activity list, a margin line that shows covered sales, phone heights by @natemsz in https://github.com/natemsz/evesynapse/pull/822
* docs(changelog): review changelog for v0.7.0.3 by @github-actions[bot] in https://github.com/natemsz/evesynapse/pull/820


**Full Changelog**: https://github.com/natemsz/evesynapse/compare/v0.7.0.3...v0.7.0.4

## [v0.7.0.3] - 2026-10-29

Automated build of EveSynapse v0.7.0.3. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).


## What's Changed
### 🚀 Features
* Add the order book chart set behind the book_depth visualization switch by @natemsz in https://github.com/natemsz/evesynapse/pull/817
### Other Changes
* docs(changelog): review changelog for v0.7.0.2 by @github-actions[bot] in https://github.com/natemsz/evesynapse/pull/815
* Profit per blueprint: realized, at the time, and at today's prices by @natemsz in https://github.com/natemsz/evesynapse/pull/814
* Profit from LP stores: inferred purchases with confidence tiers, LP income and net LP beside ISK by @natemsz in https://github.com/natemsz/evesynapse/pull/816
* Profit page charts: daily net, activity split, fees, per character, rolling margin by @natemsz in https://github.com/natemsz/evesynapse/pull/818
* Intel reader: stop reading scouts' words as pilots by @natemsz in https://github.com/natemsz/evesynapse/pull/819


**Full Changelog**: https://github.com/natemsz/evesynapse/compare/v0.7.0.2...v0.7.0.3

## [v0.7.0.2] - 2026-10-29

Automated build of EveSynapse v0.7.0.2. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).


## What's Changed
### 🚀 Features
* Market browser opens on the tree alone, unfolds categories in place, sorts by column header by @natemsz in https://github.com/natemsz/evesynapse/pull/803
* Baskets build and edit in place without a page load by @natemsz in https://github.com/natemsz/evesynapse/pull/805
* Notification links view without switching the acting character; briefing stacks with dismissals by @natemsz in https://github.com/natemsz/evesynapse/pull/795
* Market category glyphs for Blueprints, Skills and Trade Goods by @natemsz in https://github.com/natemsz/evesynapse/pull/810
* LP store: range filters, find-stores by item, coloured ISK per LP, CSV/JSON exports by @natemsz in https://github.com/natemsz/evesynapse/pull/809
* Watch list search finds baskets; a basket can be watched as a whole by @natemsz in https://github.com/natemsz/evesynapse/pull/808
* Price history chart layers, ranges and visualization switches by @natemsz in https://github.com/natemsz/evesynapse/pull/812
### 🐛 Bug Fixes
* Landing notices clear the fixed header; footer links to Service Health by @natemsz in https://github.com/natemsz/evesynapse/pull/804
* API key creation no longer asks for a fresh security-key assertion by @natemsz in https://github.com/natemsz/evesynapse/pull/807
* Solid heading text; gradient text only on the landing hero by @natemsz in https://github.com/natemsz/evesynapse/pull/811
### Other Changes
* docs(changelog): review changelog for v0.6.1.7 by @github-actions[bot] in https://github.com/natemsz/evesynapse/pull/782
* docs(changelog): review changelog for v0.6.1.8 by @github-actions[bot] in https://github.com/natemsz/evesynapse/pull/784
* docs(changelog): review changelog for v0.6.1.9 by @github-actions[bot] in https://github.com/natemsz/evesynapse/pull/789
* docs(changelog): review changelog for v0.7.0.1 by @github-actions[bot] in https://github.com/natemsz/evesynapse/pull/802
* Profit per item: pooled trading P&L with paired tax, tied broker fees and typed gaps by @natemsz in https://github.com/natemsz/evesynapse/pull/813


**Full Changelog**: https://github.com/natemsz/evesynapse/compare/v0.7.0.1...v0.7.0.2

## [v0.7.0.1] - 2026-10-29

Automated build of EveSynapse v0.7.0.1. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).
## [v0.6.1.9] - 2026-10-28

Automated build of EveSynapse v0.6.1.9. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).
## [v0.6.1.8] - 2026-10-28

Automated build of EveSynapse v0.6.1.8. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).


## What's Changed
### Other Changes
* Stops the abyssal scoring pass on shutdown instead of logging every item by @natemsz in https://github.com/natemsz/evesynapse/pull/783


**Full Changelog**: https://github.com/natemsz/evesynapse/compare/v0.6.1.6...v0.6.1.8
## [v0.6.1.7] - 2026-10-28

Automated build of EveSynapse v0.6.1.7. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).


## What's Changed
### 🚀 Features
* Ship Advanced Market Tools, the market API, admin notifications, and the service status summary at 0.7.0.1 by @natemsz in https://github.com/natemsz/evesynapse/pull/801
* Stores the market stat pack on each region stat row by @natemsz in https://github.com/natemsz/evesynapse/pull/788
* Adds a public service status summary at /status.json by @natemsz in https://github.com/natemsz/evesynapse/pull/798
* Admin global notices with sanitized posts and rate-limited publishing by @natemsz in https://github.com/natemsz/evesynapse/pull/800
### 🧰 Maintenance & Dependencies
* Bumps version to 0.7.0.0 by @natemsz in https://github.com/natemsz/evesynapse/pull/796


**Full Changelog**: https://github.com/natemsz/evesynapse/compare/v0.6.1.9...v0.7.0.1
* Make nav group labels read as headings in nav menus by @natemsz in https://github.com/natemsz/evesynapse/pull/786
### Other Changes
* Reads a system typed into a local list as the system, and files an Axon paste as the pilot at the keyboard by @natemsz in https://github.com/natemsz/evesynapse/pull/785
* Search: names resolve as you stop typing, Enter opens every kind of result with filters by @natemsz in https://github.com/natemsz/evesynapse/pull/787


**Full Changelog**: https://github.com/natemsz/evesynapse/compare/v0.6.1.8...v0.6.1.9
* Add swipe stepping to the landing gallery by @natemsz in https://github.com/natemsz/evesynapse/pull/768
* Sizes nav glyphs in rem with explicit dimensions and shortens the swap transition by @natemsz in https://github.com/natemsz/evesynapse/pull/774
* Bakes the muted backdrop grade into muted twins of the wallpaper tiers by @natemsz in https://github.com/natemsz/evesynapse/pull/775
### Other Changes
* docs(changelog): review changelog for v0.6.1.5 by @github-actions[bot] in https://github.com/natemsz/evesynapse/pull/767
* Settles a 403 on a contract exit probe instead of asking again every cycle by @natemsz in https://github.com/natemsz/evesynapse/pull/769
* Settles 403 and 422 as dead in the shared market fetch classifier by @natemsz in https://github.com/natemsz/evesynapse/pull/770
* Splits a rejected corp asset-names batch down to the bad item by @natemsz in https://github.com/natemsz/evesynapse/pull/771
* Keeps character and type name misses across restarts by @natemsz in https://github.com/natemsz/evesynapse/pull/772
* Moves the host sampler's disk read into a Linux and macOS file so the app builds on Windows by @natemsz in https://github.com/natemsz/evesynapse/pull/773
* Stamps the intel token test's chat lines with the current time by @natemsz in https://github.com/natemsz/evesynapse/pull/781
* Reuses the Forge liquid-core ranking for an hour by @natemsz in https://github.com/natemsz/evesynapse/pull/777
* Asks each side of an entity's killboard for its own page through its own index by @natemsz in https://github.com/natemsz/evesynapse/pull/778
* Has the urgent drain serve only history wants from a page open now by @natemsz in https://github.com/natemsz/evesynapse/pull/779
* Refetches market history once ESI has published a new day by @natemsz in https://github.com/natemsz/evesynapse/pull/780


**Full Changelog**: https://github.com/natemsz/evesynapse/compare/v0.6.1.5...v0.6.1.7

## [v0.6.1.5] - 2026-10-28

Automated build of EveSynapse v0.6.1.5. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).


## What's Changed
### 🚀 Features
* Give the two factor page a centered prompt layout by @natemsz in https://github.com/natemsz/evesynapse/pull/765
### 🐛 Bug Fixes
* Settle bids for no longer public contracts as dead by @natemsz in https://github.com/natemsz/evesynapse/pull/764
### Other Changes
* docs(changelog): review changelog for v0.6.1.2 by @github-actions[bot] in https://github.com/natemsz/evesynapse/pull/760
* docs(changelog): review changelog for v0.6.1.3 by @github-actions[bot] in https://github.com/natemsz/evesynapse/pull/761
* docs(changelog): review changelog for v0.6.1.4 by @github-actions[bot] in https://github.com/natemsz/evesynapse/pull/763
* docs(changelog): review changelog for v0.6.1.4 by @github-actions[bot] in https://github.com/natemsz/evesynapse/pull/766


**Full Changelog**: https://github.com/natemsz/evesynapse/compare/v0.6.1.4...v0.6.1.5

## [v0.6.1.4] - 2026-10-28

Automated build of EveSynapse v0.6.1.4. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).


## What's Changed
### 🐛 Bug Fixes
* Fail shut when the second-factor arming reads fail by @natemsz in https://github.com/natemsz/evesynapse/pull/762
### Other Changes
* docs(changelog): review changelog for v0.6.1.1 by @github-actions[bot] in https://github.com/natemsz/evesynapse/pull/754


**Full Changelog**: https://github.com/natemsz/evesynapse/compare/v0.6.1.3...v0.6.1.4

## [v0.6.1.3] - 2026-10-28

Automated build of EveSynapse v0.6.1.3. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).
## [v0.6.1.2] - 2026-10-28

Automated build of EveSynapse v0.6.1.2. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).


## What's Changed
### 🚀 Features
* Add the landing gallery by @natemsz in https://github.com/natemsz/evesynapse/pull/758
### 🐛 Bug Fixes
* Fix the briefing filter spacing on narrow screens by @natemsz in https://github.com/natemsz/evesynapse/pull/759


**Full Changelog**: https://github.com/natemsz/evesynapse/compare/v0.6.1.2...v0.6.1.3
* Moves the Sync page into the Admin page as a tab by @natemsz in https://github.com/natemsz/evesynapse/pull/755
* Add WebAuthn two-factor authentication by @natemsz in https://github.com/natemsz/evesynapse/pull/757
### 🐛 Bug Fixes
* Fix the market history freshness read in admin analytics by @natemsz in https://github.com/natemsz/evesynapse/pull/756


**Full Changelog**: https://github.com/natemsz/evesynapse/compare/v0.6.1.1...v0.6.1.2
* Add the admin analytics page by @natemsz in https://github.com/natemsz/evesynapse/pull/753
### Other Changes
* docs(changelog): review changelog for v0.6.1.0 by @github-actions[bot] in https://github.com/natemsz/evesynapse/pull/752


**Full Changelog**: https://github.com/natemsz/evesynapse/compare/v0.6.1.0...v0.6.1.1

## [v0.6.1.0] - 2026-10-28

Automated build of EveSynapse v0.6.1.0. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).


## What's Changed
### Other Changes
* docs(changelog): review changelog for v0.6.0.9 by @github-actions[bot] in https://github.com/natemsz/evesynapse/pull/750
* Axon site panel: window controls in the site's top bar, in the active theme by @natemsz in https://github.com/natemsz/evesynapse/pull/751


**Full Changelog**: https://github.com/natemsz/evesynapse/compare/v0.6.0.9...v0.6.1.0

## [v0.6.0.9] - 2026-10-27

Automated build of EveSynapse v0.6.0.9. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).


## What's Changed
### Other Changes
* docs(changelog): review changelog for v0.6.0.8 by @github-actions[bot] in https://github.com/natemsz/evesynapse/pull/746
* Workbench map: zoom to the open thread, improved projection, every thread marked by @natemsz in https://github.com/natemsz/evesynapse/pull/747
* Return to the page asked for after sign-in by @natemsz in https://github.com/natemsz/evesynapse/pull/748
* Axon intel API: threads, feed, and report breakdown for Axon's own windows by @natemsz in https://github.com/natemsz/evesynapse/pull/749


**Full Changelog**: https://github.com/natemsz/evesynapse/compare/v0.6.0.8...v0.6.0.9

## [v0.6.0.8] - 2026-10-27

Automated build of EveSynapse v0.6.0.8. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).


## What's Changed
### 🐛 Bug Fixes
* Re-wire the intel workbench on swapped-in pages by @natemsz in https://github.com/natemsz/evesynapse/pull/745
### Other Changes
* docs(changelog): review changelog for v0.6.0.7 by @github-actions[bot] in https://github.com/natemsz/evesynapse/pull/744


**Full Changelog**: https://github.com/natemsz/evesynapse/compare/v0.6.0.7...v0.6.0.8

## [v0.6.0.7] - 2026-10-27

Automated build of EveSynapse v0.6.0.7. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).


## What's Changed
### Other Changes
* docs(changelog): review changelog for v0.6.0.6 by @github-actions[bot] in https://github.com/natemsz/evesynapse/pull/743


**Full Changelog**: https://github.com/natemsz/evesynapse/compare/v0.6.0.6...v0.6.0.7

## [v0.6.0.6] - 2026-10-27

Automated build of EveSynapse v0.6.0.6. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).


## What's Changed
### 🐛 Bug Fixes
* Restores the first page with its modals when a swap returns to it by @natemsz in https://github.com/natemsz/evesynapse/pull/742
### Other Changes
* Finds a type's corpus contracts from the item type index before sorting them by @natemsz in https://github.com/natemsz/evesynapse/pull/739
* docs(changelog): review changelog for v0.6.0.5 by @github-actions[bot] in https://github.com/natemsz/evesynapse/pull/740
* Keep departed public contracts on record (schema 096) by @natemsz in https://github.com/natemsz/evesynapse/pull/741


**Full Changelog**: https://github.com/natemsz/evesynapse/compare/v0.6.0.5...v0.6.0.6

## [v0.6.0.5] - 2026-10-27

Automated build of EveSynapse v0.6.0.5. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).


## What's Changed
### 🐛 Bug Fixes
* Files the intel feed page under the intel section so it renders the section tab strip by @natemsz in https://github.com/natemsz/evesynapse/pull/736
### Other Changes
* Intel workbench dashboard, onto main (#726 merged into the threads branch instead) by @natemsz in https://github.com/natemsz/evesynapse/pull/737
* Stores market history in one statement per pair by @natemsz in https://github.com/natemsz/evesynapse/pull/733
* Reads region book pages a window at a time by @natemsz in https://github.com/natemsz/evesynapse/pull/734
* Reads contract items and bids a window at a time, stores list pages in one statement by @natemsz in https://github.com/natemsz/evesynapse/pull/735
* Reads and writes the order lifecycle ledger in one statement each by @natemsz in https://github.com/natemsz/evesynapse/pull/738
* docs(changelog): review changelog for v0.6.0.4 by @github-actions[bot] in https://github.com/natemsz/evesynapse/pull/732


**Full Changelog**: https://github.com/natemsz/evesynapse/compare/v0.6.0.4...v0.6.0.5

## [v0.6.0.4] - 2026-10-27

Automated build of EveSynapse v0.6.0.4. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).


## What's Changed
### 🚀 Features
* Sign-up theme step and Axon offer, landing frost on the header flyouts, Customize Theme modal by @natemsz in https://github.com/natemsz/evesynapse/pull/728
### 🐛 Bug Fixes
* Themes the wordmark for the Servant Sisters of EVE scheme by @natemsz in https://github.com/natemsz/evesynapse/pull/727
### Other Changes
* docs(changelog): review changelog for v0.6.0.2 by @github-actions[bot] in https://github.com/natemsz/evesynapse/pull/722
* Keep chat shorthand and place words out of pilot readings; ask the model about doubtful ones by @natemsz in https://github.com/natemsz/evesynapse/pull/723
* Axon preview endpoint and send overrides for kind and system by @natemsz in https://github.com/natemsz/evesynapse/pull/724
* Intel threads: link report groups across neighbouring systems by @natemsz in https://github.com/natemsz/evesynapse/pull/725
* Gives every flyout the bell's glass, tinted by the colour scheme by @natemsz in https://github.com/natemsz/evesynapse/pull/730
* Moves the theme and Axon steps to after the first character is added by @natemsz in https://github.com/natemsz/evesynapse/pull/731


**Full Changelog**: https://github.com/natemsz/evesynapse/compare/v0.6.0.2...v0.6.0.4

## [v0.6.0.2] - 2026-10-27

Automated build of EveSynapse v0.6.0.2. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).


## What's Changed
### 🐛 Bug Fixes
* Files a d-scan under its celestials' system when the named system is only a gate destination by @natemsz in https://github.com/natemsz/evesynapse/pull/720
### Other Changes
* docs(changelog): review changelog for v0.6.0.1 by @github-actions[bot] in https://github.com/natemsz/evesynapse/pull/719


**Full Changelog**: https://github.com/natemsz/evesynapse/compare/v0.6.0.1...v0.6.0.2

## [v0.6.0.1] - 2026-10-27

Automated build of EveSynapse v0.6.0.1. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).


## What's Changed
### 🚀 Features
* Adds the Servant Sisters of EVE colour scheme by @natemsz in https://github.com/natemsz/evesynapse/pull/718
### 🐛 Bug Fixes
* Brightens the Triglavian core red and separates the Caldari and Triglavian glows by @natemsz in https://github.com/natemsz/evesynapse/pull/717
### Other Changes
* docs(changelog): review changelog for v0.6.0.0 by @github-actions[bot] in https://github.com/natemsz/evesynapse/pull/716


**Full Changelog**: https://github.com/natemsz/evesynapse/compare/v0.6.0.0...v0.6.0.1

## [v0.6.0.0] - 2026-10-27

Automated build of EveSynapse v0.6.0.0. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).


## What's Changed
### 🚀 Features
* Mirrors the minimalistic glow to the left side shape and bumps version to v0.6.0.0 by @natemsz in https://github.com/natemsz/evesynapse/pull/715
### 🐛 Bug Fixes
* Tier buttons keep their capitals in the selected state by @natemsz in https://github.com/natemsz/evesynapse/pull/700
* Prunes intel retention daily and stops re-asking for unresolvable kill names every minute by @natemsz in https://github.com/natemsz/evesynapse/pull/701
* Skips the pilot orbit walk for characters whose snapshots have not moved by @natemsz in https://github.com/natemsz/evesynapse/pull/703
* Weighs gang candidates with two batched reads instead of two per candidate by @natemsz in https://github.com/natemsz/evesynapse/pull/704
* Trims the live intel feed to one page and gates its tick on visibility by @natemsz in https://github.com/natemsz/evesynapse/pull/705
* Reads faction names from the global snapshot instead of fetching per pilot by @natemsz in https://github.com/natemsz/evesynapse/pull/706
* Parses each stored scan once for the duplicate checks by @natemsz in https://github.com/natemsz/evesynapse/pull/707
* Skips the 90-day aggregates a pilot profile replaces by @natemsz in https://github.com/natemsz/evesynapse/pull/708
* Prices kill lines once at ingest and reads the stored prices on renders by @natemsz in https://github.com/natemsz/evesynapse/pull/709
* Derives a group composite once per input change instead of rebuilding per report by @natemsz in https://github.com/natemsz/evesynapse/pull/710
* Resolves composite fleet-link names in one query per kind by @natemsz in https://github.com/natemsz/evesynapse/pull/711
* Checks mail and calendar snapshot existence in one query per batch by @natemsz in https://github.com/natemsz/evesynapse/pull/712
* Sorts and bounds the workbench dossier read in SQL by @natemsz in https://github.com/natemsz/evesynapse/pull/713
* Batches the demo seed type resolution and multi-row inserts by @natemsz in https://github.com/natemsz/evesynapse/pull/714
* Remembers war detail state between cycles instead of re-reading every payload by @natemsz in https://github.com/natemsz/evesynapse/pull/702
### Other Changes
* docs(changelog): review changelog for v0.5.9.11 by @github-actions[bot] in https://github.com/natemsz/evesynapse/pull/699


**Full Changelog**: https://github.com/natemsz/evesynapse/compare/v0.5.9.11...v0.6.0.0

## [v0.5.9.11] - 2026-10-27

Automated build of EveSynapse v0.5.9.11. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).


## What's Changed
### 🐛 Bug Fixes
* Checks for a pilot profile before opening the fold transaction by @natemsz in https://github.com/natemsz/evesynapse/pull/697
* Retries a kill whose ingest fails instead of skipping it, and coalesces the killfeed cursor write by @natemsz in https://github.com/natemsz/evesynapse/pull/695
* Compares the map layers fingerprint before building the layers by @natemsz in https://github.com/natemsz/evesynapse/pull/698
### Other Changes
* docs(changelog): review changelog for v0.5.9.10 by @github-actions[bot] in https://github.com/natemsz/evesynapse/pull/696


**Full Changelog**: https://github.com/natemsz/evesynapse/compare/v0.5.9.10...v0.5.9.11

## [v0.5.9.10] - 2026-10-27

Automated build of EveSynapse v0.5.9.10. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).


## What's Changed
### 🚀 Features
* Warming cycle: skip repeat work, rest failing swarm pulls, split phase timing by @natemsz in https://github.com/natemsz/evesynapse/pull/684
* Float the briefing cards like At a Glance by @natemsz in https://github.com/natemsz/evesynapse/pull/693
### 🐛 Bug Fixes
* Files its heartbeat with warming peers so their directories can verify its signed pulls by @natemsz in https://github.com/natemsz/evesynapse/pull/688
* Resolves warming-cycle type names from the swarm before spending ESI by @natemsz in https://github.com/natemsz/evesynapse/pull/689
* Counts residue attempts with backoff and a dead-letter, captures the reader child stderr, splits its budgets, and reads identical report text once by @natemsz in https://github.com/natemsz/evesynapse/pull/690
* Stops the shell swapping the pre-save home page back over a saved layout by @natemsz in https://github.com/natemsz/evesynapse/pull/691
* Keeps intel reports pending when a bulk lookup fails transiently by @natemsz in https://github.com/natemsz/evesynapse/pull/694
### 🧰 Maintenance & Dependencies
* Bump github.com/tdewolff/minify/v2 from 2.24.22 to 2.24.23 by @dependabot[bot] in https://github.com/natemsz/evesynapse/pull/692
### Other Changes
* docs(changelog): review changelog for v0.5.9.9 by @github-actions[bot] in https://github.com/natemsz/evesynapse/pull/687


**Full Changelog**: https://github.com/natemsz/evesynapse/compare/v0.5.9.9...v0.5.9.10

## [v0.5.9.9] - 2026-10-26

Automated build of EveSynapse v0.5.9.9. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).


## What's Changed
### Other Changes
* docs(changelog): review changelog for v0.5.9.8 by @github-actions[bot] in https://github.com/natemsz/evesynapse/pull/683
* Anchor the backdrop glow at the top-right corner and size it off the viewport by @natemsz in https://github.com/natemsz/evesynapse/pull/685
* Replace the theme directions with a single saved-preferences note by @natemsz in https://github.com/natemsz/evesynapse/pull/686


**Full Changelog**: https://github.com/natemsz/evesynapse/compare/v0.5.9.8...v0.5.9.9

## [v0.5.9.8] - 2026-10-26

Automated build of EveSynapse v0.5.9.8. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).


## What's Changed
### 🐛 Bug Fixes
* Restore the backdrop glow, put chrome borders and card links on the scheme colour, re-hue the map glow, unbar the killboard labels, and make the Settings gear navigate by @natemsz in https://github.com/natemsz/evesynapse/pull/682
### Other Changes
* docs(changelog): review changelog for v0.5.9.7 by @github-actions[bot] in https://github.com/natemsz/evesynapse/pull/681


**Full Changelog**: https://github.com/natemsz/evesynapse/compare/v0.5.9.7...v0.5.9.8

## [v0.5.9.7] - 2026-10-26

Automated build of EveSynapse v0.5.9.7. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).


## What's Changed
### 🚀 Features
* Buttons take flat frosted glass, panel headings carry no scheme gradient, and the Settings gear gains a flyout menu by @natemsz in https://github.com/natemsz/evesynapse/pull/679
* Theme is an account setting, rendered on signed-in loads by @natemsz in https://github.com/natemsz/evesynapse/pull/680
### Other Changes
* Plan fetches by per-kind scope and settle ESI refusals once per character and kind by @natemsz in https://github.com/natemsz/evesynapse/pull/678
* docs(changelog): review changelog for v0.5.9.5 by @github-actions[bot] in https://github.com/natemsz/evesynapse/pull/675
* docs(changelog): review changelog for v0.5.9.6 by @github-actions[bot] in https://github.com/natemsz/evesynapse/pull/677


**Full Changelog**: https://github.com/natemsz/evesynapse/compare/v0.5.9.6...v0.5.9.7

## [v0.5.9.6] - 2026-10-26

Automated build of EveSynapse v0.5.9.6. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).


## What's Changed
### Other Changes
* Schemes repaint borders, the wordmark, glyphs and charts; Settings tabs survive history navigation by @natemsz in https://github.com/natemsz/evesynapse/pull/676


**Full Changelog**: https://github.com/natemsz/evesynapse/compare/v0.5.9.5...v0.5.9.6
## [v0.5.9.5] - 2026-10-26

Automated build of EveSynapse v0.5.9.5. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).


## What's Changed
### 🚀 Features
* Add the tabbed Settings page with colour schemes by @natemsz in https://github.com/natemsz/evesynapse/pull/674
### 🐛 Bug Fixes
* Honor the Axon lines API's batch speaker by @natemsz in https://github.com/natemsz/evesynapse/pull/673
### Other Changes
* docs(changelog): review changelog for v0.5.9.4 by @github-actions[bot] in https://github.com/natemsz/evesynapse/pull/672


**Full Changelog**: https://github.com/natemsz/evesynapse/compare/v0.5.9.4...v0.5.9.5

## [v0.5.9.4] - 2026-10-25

Automated build of EveSynapse v0.5.9.4. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).


## What's Changed
### 🐛 Bug Fixes
* Read near-miss system names as locations wherever a line speaks of a place by @natemsz in https://github.com/natemsz/evesynapse/pull/670
* Store no-system roster lines on the pilots EVE verifies by @natemsz in https://github.com/natemsz/evesynapse/pull/671
### Other Changes
* docs(changelog): review changelog for v0.5.9.3 by @github-actions[bot] in https://github.com/natemsz/evesynapse/pull/669


**Full Changelog**: https://github.com/natemsz/evesynapse/compare/v0.5.9.3...v0.5.9.4

## [v0.5.9.3] - 2026-10-25

Automated build of EveSynapse v0.5.9.3. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).


## What's Changed
### 🚀 Features
* Add the residue corpus builder by @natemsz in https://github.com/natemsz/evesynapse/pull/668
### 🐛 Bug Fixes
* File gate readings under both systems and read loose gate phrasings by @natemsz in https://github.com/natemsz/evesynapse/pull/667
### Other Changes
* Resolve a name-less channel line's system from its speaker first; link workbench pilot names to their pilot pages by @natemsz in https://github.com/natemsz/evesynapse/pull/666
* docs(changelog): review changelog for v0.5.9.2 by @github-actions[bot] in https://github.com/natemsz/evesynapse/pull/665


**Full Changelog**: https://github.com/natemsz/evesynapse/compare/v0.5.9.2...v0.5.9.3

## [v0.5.9.2] - 2026-10-25

Automated build of EveSynapse v0.5.9.2. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).


## What's Changed
### Other Changes
* docs(changelog): review changelog for v0.5.9.1 by @github-actions[bot] in https://github.com/natemsz/evesynapse/pull/663
* Cycle the page backdrop from the Theme button: Nebula, Muted, Ember glow by @natemsz in https://github.com/natemsz/evesynapse/pull/664


**Full Changelog**: https://github.com/natemsz/evesynapse/compare/v0.5.9.1...v0.5.9.2

## [v0.5.9.1] - 2026-10-25

Automated build of EveSynapse v0.5.9.1. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).


## What's Changed
### Other Changes
* docs(changelog): review changelog for v0.5.9.0 by @github-actions[bot] in https://github.com/natemsz/evesynapse/pull/660
* Restore the landing bar glass, hoist the Tools panel, glass the hero buttons, sweep the residue backlog by @natemsz in https://github.com/natemsz/evesynapse/pull/662


**Full Changelog**: https://github.com/natemsz/evesynapse/compare/v0.5.9.0...v0.5.9.1

## [v0.5.9.0] - 2026-10-24

Automated build of EveSynapse v0.5.9.0. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).


## What's Changed
### 🚀 Features
* Intel: residue step reads the leftover spans of settled reports with a local reader by @natemsz in https://github.com/natemsz/evesynapse/pull/658
### Other Changes
* docs(changelog): review changelog for v0.5.8.3 by @github-actions[bot] in https://github.com/natemsz/evesynapse/pull/657
* Landing menu: frost on its own backdrop-filter, bar glass on its own layer, thinner panel edge by @natemsz in https://github.com/natemsz/evesynapse/pull/659


**Full Changelog**: https://github.com/natemsz/evesynapse/compare/v0.5.8.3...v0.5.9.0

## [v0.5.8.3] - 2026-10-24

Automated build of EveSynapse v0.5.8.3. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).


## What's Changed
### 🐛 Bug Fixes
* Intel analyzer: read scan-shaped text as rows, set name columns aside by @natemsz in https://github.com/natemsz/evesynapse/pull/655
* Warming: stop junk ESI calls, settle definitive 404s as dead by @natemsz in https://github.com/natemsz/evesynapse/pull/656
### Other Changes
* docs(changelog): review changelog for v0.5.8.2 by @github-actions[bot] in https://github.com/natemsz/evesynapse/pull/650
* Landing nav: headline tools as direct links by @natemsz in https://github.com/natemsz/evesynapse/pull/651
* Gate camps name their gate: celestial attribution and recurring crew by @natemsz in https://github.com/natemsz/evesynapse/pull/652
* Landing: categorized Tools dropdown and full-width top bar by @natemsz in https://github.com/natemsz/evesynapse/pull/654


**Full Changelog**: https://github.com/natemsz/evesynapse/compare/v0.5.8.2...v0.5.8.3

## [v0.5.8.2] - 2026-10-24

Automated build of EveSynapse v0.5.8.2. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).


## What's Changed
### 🚀 Features
* Pilot activity profiles from kill data by @natemsz in https://github.com/natemsz/evesynapse/pull/648
### Other Changes
* docs(changelog): review changelog for v0.5.8.1 by @github-actions[bot] in https://github.com/natemsz/evesynapse/pull/645
* Intel channels: report groups, stored composites, residue capture by @natemsz in https://github.com/natemsz/evesynapse/pull/646
* Landing: hero lowered toward the planet, Tools opens public tools, top bar wears the liquid glass (demo) by @natemsz in https://github.com/natemsz/evesynapse/pull/647
* Intel fusion P2: tracked gangs, confidence decay, likely ships from profiles by @natemsz in https://github.com/natemsz/evesynapse/pull/649


**Full Changelog**: https://github.com/natemsz/evesynapse/compare/v0.5.8.1...v0.5.8.2

## [v0.5.8.1] - 2026-10-24

Automated build of EveSynapse v0.5.8.1. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).


## What's Changed
### Other Changes
* docs(changelog): review changelog for v0.5.8.0 by @github-actions[bot] in https://github.com/natemsz/evesynapse/pull/643
* Workbench: fix snapshot head floating over the page, widen box spacing by @natemsz in https://github.com/natemsz/evesynapse/pull/644


**Full Changelog**: https://github.com/natemsz/evesynapse/compare/v0.5.8.0...v0.5.8.1

## [v0.5.8.0] - 2026-10-24

Automated build of EveSynapse v0.5.8.0. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).


## What's Changed
### 🚀 Features
* Intel Workbench: the per-report page (scan door built, inactive) by @natemsz in https://github.com/natemsz/evesynapse/pull/642
### 🐛 Bug Fixes
* Landing: restore hero geometry after the companion block move by @natemsz in https://github.com/natemsz/evesynapse/pull/640
* Intel: rescue fused rosters on the chat-lines path, backfill stored reports by @natemsz in https://github.com/natemsz/evesynapse/pull/641
### Other Changes
* docs(changelog): review changelog for v0.5.7.9 by @github-actions[bot] in https://github.com/natemsz/evesynapse/pull/639


**Full Changelog**: https://github.com/natemsz/evesynapse/compare/v0.5.7.9...v0.5.8.0

## [v0.5.7.9] - 2026-10-24

Automated build of EveSynapse v0.5.7.9. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).


## What's Changed
### 🐛 Bug Fixes
* Landing: move the companion block to the top of the hero by @natemsz in https://github.com/natemsz/evesynapse/pull/638
### Other Changes
* docs(changelog): review changelog for v0.5.7.8 by @github-actions[bot] in https://github.com/natemsz/evesynapse/pull/637


**Full Changelog**: https://github.com/natemsz/evesynapse/compare/v0.5.7.8...v0.5.7.9

## [v0.5.7.8] - 2026-10-24

Automated build of EveSynapse v0.5.7.8. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).


## What's Changed
### 🚀 Features
* Intel feed: standing-split bar, linked hulls and gates, system doors by @natemsz in https://github.com/natemsz/evesynapse/pull/633
### 🐛 Bug Fixes
* Landing: adopt the owner's reference copy; restore his apart, trust and playstyles wording by @natemsz in https://github.com/natemsz/evesynapse/pull/634
* Landing: remove the What sets us apart section by @natemsz in https://github.com/natemsz/evesynapse/pull/635
* Intel: read fused and spaced local rosters as rosters; dedupe by pilot set; poster-location system fallback; scan page head fix by @natemsz in https://github.com/natemsz/evesynapse/pull/636
### Other Changes
* docs(changelog): review changelog for v0.5.7.7 by @github-actions[bot] in https://github.com/natemsz/evesynapse/pull/632


**Full Changelog**: https://github.com/natemsz/evesynapse/compare/v0.5.7.7...v0.5.7.8

## [v0.5.7.7] - 2026-10-24

Automated build of EveSynapse v0.5.7.7. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).


## What's Changed
### 🚀 Features
* D-scan parity step 3: groups, compare, OG previews, system correction, hull pricing by @natemsz in https://github.com/natemsz/evesynapse/pull/631
### 🐛 Bug Fixes
* Intel parser: things are not pilots; scan pastes classified; report headlines synthesized by @natemsz in https://github.com/natemsz/evesynapse/pull/630
### Other Changes
* docs(changelog): review changelog for v0.5.7.6 by @github-actions[bot] in https://github.com/natemsz/evesynapse/pull/629


**Full Changelog**: https://github.com/natemsz/evesynapse/compare/v0.5.7.6...v0.5.7.7

## [v0.5.7.6] - 2026-10-23

Automated build of EveSynapse v0.5.7.6. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).


## What's Changed
### 🚀 Features
* Intel feed page, bare hostile counts in chat parsing, skipped lines named to the poster by @natemsz in https://github.com/natemsz/evesynapse/pull/624
* Landing page: simple structure, three doors, factual differentiators by @natemsz in https://github.com/natemsz/evesynapse/pull/625
* D-scan parity step 1: the reading itself by @natemsz in https://github.com/natemsz/evesynapse/pull/626
* Pilot page: intel section moves to a right rail when the pilot has intel by @natemsz in https://github.com/natemsz/evesynapse/pull/628
### Other Changes
* docs(changelog): review changelog for v0.5.7.5 by @github-actions[bot] in https://github.com/natemsz/evesynapse/pull/623


**Full Changelog**: https://github.com/natemsz/evesynapse/compare/v0.5.7.5...v0.5.7.6

## [v0.5.7.5] - 2026-10-23

Automated build of EveSynapse v0.5.7.5. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).


## What's Changed
### 🚀 Features
* Landing: one display for the module copy by @natemsz in https://github.com/natemsz/evesynapse/pull/615
* Report a bug page by @natemsz in https://github.com/natemsz/evesynapse/pull/617
* System page: recent kills, gate camp flag, intel reports, map link by @natemsz in https://github.com/natemsz/evesynapse/pull/622
### 🐛 Bug Fixes
* Map: territory outline and region view follow the projection setting by @natemsz in https://github.com/natemsz/evesynapse/pull/614
* Planner: recalculate works again — dialog controls keep their form by @natemsz in https://github.com/natemsz/evesynapse/pull/616
* Harden /healthz, pairing and the Axon API; link kills in-app by @natemsz in https://github.com/natemsz/evesynapse/pull/618
* Gate camps: ship kills only; composition counts distinct pilots by @natemsz in https://github.com/natemsz/evesynapse/pull/619
* True values: price contract-only types from contract evidence, never from build cost by @natemsz in https://github.com/natemsz/evesynapse/pull/620
* Landing hero: headline alone beside the planet, support copy drops to the dark band by @natemsz in https://github.com/natemsz/evesynapse/pull/621
### Other Changes
* docs(changelog): review changelog for v0.5.7.4 by @github-actions[bot] in https://github.com/natemsz/evesynapse/pull/613


**Full Changelog**: https://github.com/natemsz/evesynapse/compare/v0.5.7.4...v0.5.7.5

## [v0.5.7.4] - 2026-10-23

Automated build of EveSynapse v0.5.7.4. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).


## What's Changed
### 🚀 Features
* Map: the improved projection is the SDE authored 2D layout, and the default view by @natemsz in https://github.com/natemsz/evesynapse/pull/609
### 🐛 Bug Fixes
* Learn the swarm release library from GitHub releases by @natemsz in https://github.com/natemsz/evesynapse/pull/610
* Landing: repin copy assertions to the current page text by @natemsz in https://github.com/natemsz/evesynapse/pull/612
### Other Changes
* docs(changelog): review changelog for v0.5.7.3 by @github-actions[bot] in https://github.com/natemsz/evesynapse/pull/611


**Full Changelog**: https://github.com/natemsz/evesynapse/compare/v0.5.7.3...v0.5.7.4

## [v0.5.7.3] - 2026-10-22

Automated build of EveSynapse v0.5.7.3. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).


## What's Changed
### 🚀 Features
* Landing starfield: bake the star scatter into a static tile by @natemsz in https://github.com/natemsz/evesynapse/pull/605
* Landing: header pill flush to the top of the viewport by @natemsz in https://github.com/natemsz/evesynapse/pull/607
### 🐛 Bug Fixes
* Map: zoom-tier label discipline for the tactical projection by @natemsz in https://github.com/natemsz/evesynapse/pull/606
### Other Changes
* docs(changelog): review changelog for v0.5.7.2 by @github-actions[bot] in https://github.com/natemsz/evesynapse/pull/604


**Full Changelog**: https://github.com/natemsz/evesynapse/compare/v0.5.7.2...v0.5.7.3

## [v0.5.7.2] - 2026-10-22

Automated build of EveSynapse v0.5.7.2. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).


## What's Changed
### 🚀 Features
* Landing follow-ups: smoother fade, shorter Intel and reference, white eyebrows, terser copy, new screenshots by @natemsz in https://github.com/natemsz/evesynapse/pull/603
### Other Changes
* docs(changelog): review changelog for v0.5.7.1 by @github-actions[bot] in https://github.com/natemsz/evesynapse/pull/602


**Full Changelog**: https://github.com/natemsz/evesynapse/compare/v0.5.7.1...v0.5.7.2

## [v0.5.7.1] - 2026-10-22

Automated build of EveSynapse v0.5.7.1. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).


## What's Changed
### 🚀 Features
* Landing: ember redesign (planet band, floating header, gradient headlines, public tools modal) by @natemsz in https://github.com/natemsz/evesynapse/pull/601
### Other Changes
* docs(changelog): review changelog for v0.5.7.0 by @github-actions[bot] in https://github.com/natemsz/evesynapse/pull/600


**Full Changelog**: https://github.com/natemsz/evesynapse/compare/v0.5.7.0...v0.5.7.1

## [v0.5.7.0] - 2026-10-22

Automated build of EveSynapse v0.5.7.0. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).


## What's Changed
### 🚀 Features
* feat:Map: Improved Projection, a precomputed relaxed universe layout by @natemsz in https://github.com/natemsz/evesynapse/pull/599
### Other Changes
* docs(changelog): review changelog for v0.5.6.9 by @github-actions[bot] in https://github.com/natemsz/evesynapse/pull/598


**Full Changelog**: https://github.com/natemsz/evesynapse/compare/v0.5.6.9...v0.5.7.0

## [v0.5.6.9] - 2026-10-22

Automated build of EveSynapse v0.5.6.9. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).


## What's Changed
### Other Changes
* docs(changelog): review changelog for v0.5.6.8 by @github-actions[bot] in https://github.com/natemsz/evesynapse/pull/595
* Map: names stop flickering while zooming by @natemsz in https://github.com/natemsz/evesynapse/pull/596
* Map: smooth sov territory (painted once, moved as a picture) by @natemsz in https://github.com/natemsz/evesynapse/pull/597


**Full Changelog**: https://github.com/natemsz/evesynapse/compare/v0.5.6.8...v0.5.6.9

## [v0.5.6.8] - 2026-10-22

Automated build of EveSynapse v0.5.6.8. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).


## What's Changed
### 🚀 Features
* D-scan: full glass treatment for the scan surfaces by @natemsz in https://github.com/natemsz/evesynapse/pull/580
* Killboard: complete the glass treatment on every panel surface by @natemsz in https://github.com/natemsz/evesynapse/pull/581
* Landing: name the shipped intel tools in the inventory; stage the intel route capture by @natemsz in https://github.com/natemsz/evesynapse/pull/587
* Landing: tools inventory up top, tabbed feature explorer by @natemsz in https://github.com/natemsz/evesynapse/pull/592
### 🐛 Bug Fixes
* Menus: full capitals instead of small caps by @natemsz in https://github.com/natemsz/evesynapse/pull/579
* Rename the demo pilot to Sable Iversen by @natemsz in https://github.com/natemsz/evesynapse/pull/582
* Demo notice: link your characters for full functionality by @natemsz in https://github.com/natemsz/evesynapse/pull/590
* Tab navigation at 12px, weight 500, regular letter spacing by @natemsz in https://github.com/natemsz/evesynapse/pull/593
### 🧰 Maintenance & Dependencies
* Plainer wording across the app text by @natemsz in https://github.com/natemsz/evesynapse/pull/584
### Other Changes
* docs(changelog): review changelog for v0.5.6.7 by @github-actions[bot] in https://github.com/natemsz/evesynapse/pull/578
* Map: no more blank map after going back or switching characters by @natemsz in https://github.com/natemsz/evesynapse/pull/583
* Map: kills and gate camp reports on the system card by @natemsz in https://github.com/natemsz/evesynapse/pull/585
* Map: crisp sov borders, contiguous holdings fill in, faster builds by @natemsz in https://github.com/natemsz/evesynapse/pull/586
* Intel channels: corp and alliance channels, shared by the same rules as Discord roles by @natemsz in https://github.com/natemsz/evesynapse/pull/588
* Map: routes don't avoid systems just because of intel by @natemsz in https://github.com/natemsz/evesynapse/pull/589
* Intel: understand each report (pilots, ships, gate, kind, friend or foe) by @natemsz in https://github.com/natemsz/evesynapse/pull/591


**Full Changelog**: https://github.com/natemsz/evesynapse/compare/v0.5.6.7...v0.5.6.8

## [v0.5.6.7] - 2026-10-22

Automated build of EveSynapse v0.5.6.7. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).


## What's Changed
### 🚀 Features
* Stacked page cards keep the standard section gap by @natemsz in https://github.com/natemsz/evesynapse/pull/576
### Other Changes
* docs(changelog): review changelog for v0.5.6.6 by @github-actions[bot] in https://github.com/natemsz/evesynapse/pull/575
* Map: sov territory builds faster and never mid-gesture by @natemsz in https://github.com/natemsz/evesynapse/pull/577


**Full Changelog**: https://github.com/natemsz/evesynapse/compare/v0.5.6.6...v0.5.6.7

## [v0.5.6.6] - 2026-10-22

Automated build of EveSynapse v0.5.6.6. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).


## What's Changed
### 🚀 Features
* Typography foundation: small-caps Rajdhani menus, Inter prose role, bold pass by @natemsz in https://github.com/natemsz/evesynapse/pull/572
### Other Changes
* docs(changelog): review changelog for v0.5.6.5 by @github-actions[bot] in https://github.com/natemsz/evesynapse/pull/571
* Map: re-route visibly, fly to the route, pick the system with something on it by @natemsz in https://github.com/natemsz/evesynapse/pull/573
* Map: merged territory, shareable views, flat buttons, clearer type by @natemsz in https://github.com/natemsz/evesynapse/pull/574


**Full Changelog**: https://github.com/natemsz/evesynapse/compare/v0.5.6.5...v0.5.6.6

## [v0.5.6.5] - 2026-10-21

Automated build of EveSynapse v0.5.6.5. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).


## What's Changed
### Other Changes
* docs(changelog): review changelog for v0.5.6.4 by @github-actions[bot] in https://github.com/natemsz/evesynapse/pull/569
* Map: Data menu no longer cut off on the right by @natemsz in https://github.com/natemsz/evesynapse/pull/570


**Full Changelog**: https://github.com/natemsz/evesynapse/compare/v0.5.6.4...v0.5.6.5

## [v0.5.6.4] - 2026-10-21

Automated build of EveSynapse v0.5.6.4. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).


## What's Changed
### Other Changes
* docs(changelog): review changelog for v0.5.6.3 by @github-actions[bot] in https://github.com/natemsz/evesynapse/pull/564
* Map: Data menu no longer cut off, pins readable over any layer by @natemsz in https://github.com/natemsz/evesynapse/pull/565
* Intel channels: clearer Axon setup, site styling, nav renames by @natemsz in https://github.com/natemsz/evesynapse/pull/566
* Axon links open the app, not a browser page by @natemsz in https://github.com/natemsz/evesynapse/pull/567
* Map: Intel off really looks off; heat layers give the map back by @natemsz in https://github.com/natemsz/evesynapse/pull/568


**Full Changelog**: https://github.com/natemsz/evesynapse/compare/v0.5.6.2...v0.5.6.4

## [v0.5.6.3] - 2026-10-21

Automated build of EveSynapse v0.5.6.3. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).


## What's Changed
### Other Changes
* Map: mousewheel zooms, never pans by @natemsz in https://github.com/natemsz/evesynapse/pull/559
* docs(changelog): review changelog for v0.5.6.1 by @github-actions[bot] in https://github.com/natemsz/evesynapse/pull/560
* Link running Axon from Intel, rename Log watcher to EveSynapse Axon by @natemsz in https://github.com/natemsz/evesynapse/pull/561
* Pilot watch: validate the return path with safeLocalNext by @natemsz in https://github.com/natemsz/evesynapse/pull/562
* Axon: one-file installer downloads, no install scripts by @natemsz in https://github.com/natemsz/evesynapse/pull/563


**Full Changelog**: https://github.com/natemsz/evesynapse/compare/v0.5.6.1...v0.5.6.3

## [v0.5.6.1] - 2026-10-21

Automated build of EveSynapse v0.5.6.1. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).


## What's Changed
### 🐛 Bug Fixes
* Fix: gate the Sync page link to admins so testers don't hit a dead end by @natemsz in https://github.com/natemsz/evesynapse/pull/557
### Other Changes
* docs(changelog): review changelog for v0.5.6.0 by @github-actions[bot] in https://github.com/natemsz/evesynapse/pull/553
* Map: full-screen brand watermark and a screenshot button by @natemsz in https://github.com/natemsz/evesynapse/pull/554
* Map: Data layers (your pilots, fleet, clones, orders, industry, structures, assets, PvP, mining, posts) by @natemsz in https://github.com/natemsz/evesynapse/pull/555
* Map: jump planner (SDE ranges, your skills, fatigue) and Ansiblex bridges in routing by @natemsz in https://github.com/natemsz/evesynapse/pull/556
* Intel: hostile pilot tracking (pilot activity, known hostiles per channel, watched pilots on the map) by @natemsz in https://github.com/natemsz/evesynapse/pull/558


**Full Changelog**: https://github.com/natemsz/evesynapse/compare/v0.5.6.0...v0.5.6.1

## [v0.5.6.0] - 2026-10-21

Automated build of EveSynapse v0.5.6.0. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).


## What's Changed
### 🚀 Features
* Demo account: shared test-drive pilot with seeded data and write safeguards by @natemsz in https://github.com/natemsz/evesynapse/pull/547
### Other Changes
* docs(changelog): review changelog for v0.5.5.4 by @github-actions[bot] in https://github.com/natemsz/evesynapse/pull/546
* Fix: swapped pages keep their stylesheets and scripts; swap covers the remaining pages by @natemsz in https://github.com/natemsz/evesynapse/pull/548
* Map: sov territory view, layer scale, full screen; popover and card agree by @natemsz in https://github.com/natemsz/evesynapse/pull/549
* Intel: scan tool -- d-scan, local, fleet and probe pastes, on the map card by @natemsz in https://github.com/natemsz/evesynapse/pull/550
* Intel channels: owned, granted streams of intel reports, on the map by @natemsz in https://github.com/natemsz/evesynapse/pull/551
* Intel channels: posting tokens and API for the desktop log watcher by @natemsz in https://github.com/natemsz/evesynapse/pull/552


**Full Changelog**: https://github.com/natemsz/evesynapse/compare/v0.5.5.4...v0.5.6.0

## [v0.5.5.4] - 2026-10-21

Automated build of EveSynapse v0.5.5.4. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).


## What's Changed
### 🚀 Features
* Place pages: our map links, activity stats and sovereignty by @natemsz in https://github.com/natemsz/evesynapse/pull/543
* Landing: map, killboard and route planner join the showcase by @natemsz in https://github.com/natemsz/evesynapse/pull/544
### 🐛 Bug Fixes
* Fix: abyssal roll sliders activate on family selection by @natemsz in https://github.com/natemsz/evesynapse/pull/545
### Other Changes
* docs(changelog): review changelog for v0.5.5.3 by @github-actions[bot] in https://github.com/natemsz/evesynapse/pull/542


**Full Changelog**: https://github.com/natemsz/evesynapse/compare/v0.5.5.3...v0.5.5.4

## [v0.5.5.3] - 2026-10-21

Automated build of EveSynapse v0.5.5.3. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).


## What's Changed
### 🐛 Bug Fixes
* Fix: footer sits 0.5px above the phone bottom bar by @natemsz in https://github.com/natemsz/evesynapse/pull/537
### Other Changes
* docs(changelog): review changelog for v0.5.5.2 by @github-actions[bot] in https://github.com/natemsz/evesynapse/pull/535
* Map: steady labels while zooming; zoom in never exits a region by @natemsz in https://github.com/natemsz/evesynapse/pull/536
* Killboard ingest: zKillboard R2Z2 stream + our characters' kills, priced by our appraisal by @natemsz in https://github.com/natemsz/evesynapse/pull/539
* Map: threat layer + smart routing with ESI waypoint send by @natemsz in https://github.com/natemsz/evesynapse/pull/541


**Full Changelog**: https://github.com/natemsz/evesynapse/compare/v0.5.5.2...v0.5.5.3

## [v0.5.5.2] - 2026-10-21

Automated build of EveSynapse v0.5.5.2. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).


## What's Changed
### 🚀 Features
* Feat: footer as dark black glass, star tile removed by @natemsz in https://github.com/natemsz/evesynapse/pull/534
### 🐛 Bug Fixes
* Fix: restore glass blur on touch devices by @natemsz in https://github.com/natemsz/evesynapse/pull/533
### Other Changes
* docs(changelog): review changelog for v0.5.5.1 by @github-actions[bot] in https://github.com/natemsz/evesynapse/pull/532

## New Contributors
* @github-actions[bot] made their first contribution in https://github.com/natemsz/evesynapse/pull/532

**Full Changelog**: https://github.com/natemsz/evesynapse/compare/v0.5.5.1...v0.5.5.2

## [v0.5.5.1] - 2026-10-21

Automated build of EveSynapse v0.5.5.1. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).


## What's Changed
### 🚀 Features
* Feature: abyssal roll-attribute sliders in contract search by @natemsz in https://github.com/natemsz/evesynapse/pull/528
### 🧰 Maintenance & Dependencies
* Fix: changelog automation by @natemsz in https://github.com/natemsz/evesynapse/pull/529
* Chore: backfill CHANGELOG.md from published release notes by @natemsz in https://github.com/natemsz/evesynapse/pull/530
### Other Changes
* Map: click a region name for tactical; zoom entry becomes a preference by @natemsz in https://github.com/natemsz/evesynapse/pull/531


**Full Changelog**: https://github.com/natemsz/evesynapse/compare/v0.5.5.0...v0.5.5.1

## [v0.5.5.0] - 2026-10-21

Automated build of EveSynapse v0.5.5.0. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).


## What's Changed
### 🐛 Bug Fixes
* Fix: Argon re-attack residuals (1.1, 1.8c, 2.8e, 3.2.2, 4.1c, 4.2) by @natemsz in https://github.com/natemsz/evesynapse/pull/525
### Other Changes
* Universe map: New Eden top-down with tactical region views by @natemsz in https://github.com/natemsz/evesynapse/pull/526
* Map: smooth zoom, touchpad gestures, full keyboard by @natemsz in https://github.com/natemsz/evesynapse/pull/527


**Full Changelog**: https://github.com/natemsz/evesynapse/compare/v0.5.4.8...v0.5.5.0

## [v0.5.4.8] - 2026-10-21

Automated build of EveSynapse v0.5.4.8. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).


## What's Changed
### 🐛 Bug Fixes
* Fix: footer star tile, lossless by @natemsz in https://github.com/natemsz/evesynapse/pull/523
### Other Changes
* Abyssal: rolled attributes only (display, scoring, features); drones size by bandwidth by @natemsz in https://github.com/natemsz/evesynapse/pull/524


**Full Changelog**: https://github.com/natemsz/evesynapse/compare/v0.5.4.7...v0.5.4.8

## [v0.5.4.7] - 2026-10-21

Automated build of EveSynapse v0.5.4.7. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).


## What's Changed
### Other Changes
* Abyssal ladder: bound both ends, close the top with the perfect roll by @natemsz in https://github.com/natemsz/evesynapse/pull/522


**Full Changelog**: https://github.com/natemsz/evesynapse/compare/v0.5.4.6...v0.5.4.7

## [v0.5.4.6] - 2026-10-21

Automated build of EveSynapse v0.5.4.6. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).


## What's Changed
### 🐛 Bug Fixes
* Fix: restore pre-#518 page sizing (grid shell), glass blur back to 24px by @natemsz in https://github.com/natemsz/evesynapse/pull/520
* Fix: footer star tile rebuilt stars-only, 1024px lossless WebP by @natemsz in https://github.com/natemsz/evesynapse/pull/521
### Other Changes
* Abyssal learned correction, confirmed contract sales, per-path recreation cost by @natemsz in https://github.com/natemsz/evesynapse/pull/519


**Full Changelog**: https://github.com/natemsz/evesynapse/compare/v0.5.4.5...v0.5.4.6

## [v0.5.4.5] - 2026-10-20

Automated build of EveSynapse v0.5.4.5. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).


## What's Changed
### 🚀 Features
* Landing copy pass: hidden try-it button, dashboard, trust, free-forever by @natemsz in https://github.com/natemsz/evesynapse/pull/516
### 🐛 Bug Fixes
* Phone sheet: cap the character grid, never scroll by @natemsz in https://github.com/natemsz/evesynapse/pull/517
* Sticky footer for the app shell; star-tile footer background by @natemsz in https://github.com/natemsz/evesynapse/pull/518


**Full Changelog**: https://github.com/natemsz/evesynapse/compare/v0.5.4.4...v0.5.4.5

## [v0.5.4.4] - 2026-10-20

Automated build of EveSynapse v0.5.4.4. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).


## What's Changed
### Other Changes
* Contracts: abyssal valuation, drawer stats, contract page rebuilt from drawer cards by @natemsz in https://github.com/natemsz/evesynapse/pull/515


**Full Changelog**: https://github.com/natemsz/evesynapse/compare/v0.5.4.3...v0.5.4.4

## [v0.5.4.3] - 2026-10-20

Automated build of EveSynapse v0.5.4.3. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).


## What's Changed
### Other Changes
* Mobile pass 4: native phone layer, table cards, phone sheets by @natemsz in https://github.com/natemsz/evesynapse/pull/514


**Full Changelog**: https://github.com/natemsz/evesynapse/compare/v0.5.4.2...v0.5.4.3

## [v0.5.4.2] - 2026-10-20

Automated build of EveSynapse v0.5.4.2. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).


## What's Changed
### 🚀 Features
* Mobile pass 3: phone bottom bar, sections sheet, More wrapper guard by @natemsz in https://github.com/natemsz/evesynapse/pull/512
* Fitting tool: phone top declutter, module search behind the tab bar toggle by @natemsz in https://github.com/natemsz/evesynapse/pull/513


**Full Changelog**: https://github.com/natemsz/evesynapse/compare/v0.5.4.1...v0.5.4.2

## [v0.5.4.1] - 2026-10-20

Automated build of EveSynapse v0.5.4.1. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).


## What's Changed
### 🚀 Features
* Mobile pass 2: appraisal phone treatment, home KPI compact figures by @natemsz in https://github.com/natemsz/evesynapse/pull/505
* Landing: add the sync load-test stat by @natemsz in https://github.com/natemsz/evesynapse/pull/506
* Mobile pass 2: contract results as cards at phone widths by @natemsz in https://github.com/natemsz/evesynapse/pull/509
* Mobile pass 2: section tab priority overflow (More disclosure) by @natemsz in https://github.com/natemsz/evesynapse/pull/510
* Fitting tool: phone layout (Slot Racks / Resists & Cap / Radial) by @natemsz in https://github.com/natemsz/evesynapse/pull/511
### 🐛 Bug Fixes
* Landing: remove the star layers (Firefox ghosting fix) by @natemsz in https://github.com/natemsz/evesynapse/pull/507
* My Loadouts preview: keep the panel inside the viewport left edge by @natemsz in https://github.com/natemsz/evesynapse/pull/508
### 🧰 Maintenance & Dependencies
* Sync-side drain harness (TestSyncDrain) by @natemsz in https://github.com/natemsz/evesynapse/pull/504


**Full Changelog**: https://github.com/natemsz/evesynapse/compare/v0.5.4.0...v0.5.4.1

## [v0.5.4.0] - 2026-10-20

Automated build of EveSynapse v0.5.4.0. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).


## What's Changed
### 🚀 Features
* Mobile pass 1: intermediate widths, responsive primitives, contract search pilot by @natemsz in https://github.com/natemsz/evesynapse/pull/502
### 🐛 Bug Fixes
* ocikms: do not send x-content-sha256 on bodyless requests by @natemsz in https://github.com/natemsz/evesynapse/pull/503


**Full Changelog**: https://github.com/natemsz/evesynapse/compare/v0.5.3.9...v0.5.4.0

## [v0.5.3.9] - 2026-10-20

Automated build of EveSynapse v0.5.3.9. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).


## What's Changed
### 🚀 Features
* KMS phase 2: working credentials from a secrets service, memory only by @natemsz in https://github.com/natemsz/evesynapse/pull/498
* Character sheet: skills trained since last visit banner by @natemsz in https://github.com/natemsz/evesynapse/pull/499
* My Loadouts: Modules and cargo preview popover by @natemsz in https://github.com/natemsz/evesynapse/pull/500
### 🐛 Bug Fixes
* Landing copy pass: direct hero, slogans removed, load-tested stat by @natemsz in https://github.com/natemsz/evesynapse/pull/497
### 🧰 Maintenance & Dependencies
* chore(deps): bump actions/checkout from 4 to 7 by @dependabot[bot] in https://github.com/natemsz/evesynapse/pull/493
* chore(deps): bump github.com/go-chi/chi/v5 from 5.3.2 to 5.5.0 by @dependabot[bot] in https://github.com/natemsz/evesynapse/pull/494
* chore(deps): bump peter-evans/create-pull-request from 6 to 8 by @dependabot[bot] in https://github.com/natemsz/evesynapse/pull/495
* Document the KMS cutover in the README by @natemsz in https://github.com/natemsz/evesynapse/pull/496
* CSS delivery: minified stylesheets at serve time by @natemsz in https://github.com/natemsz/evesynapse/pull/501


**Full Changelog**: https://github.com/natemsz/evesynapse/compare/v0.5.3.8...v0.5.3.9

## [v0.5.3.8] - 2026-10-19

Automated build of EveSynapse v0.5.3.8. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).


## What's Changed
### 🐛 Bug Fixes
* Fix the OCI federation exchange to the real protocol by @natemsz in https://github.com/natemsz/evesynapse/pull/492


**Full Changelog**: https://github.com/natemsz/evesynapse/compare/v0.5.3.7...v0.5.3.8

## [v0.5.3.7] - 2026-10-19

Automated build of EveSynapse v0.5.3.7. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).


## What's Changed
### 🐛 Bug Fixes
* Fix the OCI tenancy parse for the real instance certificate shape by @natemsz in https://github.com/natemsz/evesynapse/pull/491


**Full Changelog**: https://github.com/natemsz/evesynapse/compare/v0.5.3.6...v0.5.3.7

## [v0.5.3.6] - 2026-10-19

Automated build of EveSynapse v0.5.3.6. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).


## What's Changed
### 🚀 Features
* Add PKCE to the Discord OAuth flow by @natemsz in https://github.com/natemsz/evesynapse/pull/487
* Token key providers: env, KMS unwrap endpoint, or none (plus setup guidance) by @natemsz in https://github.com/natemsz/evesynapse/pull/488
* Add the OCI KMS token key provider by @natemsz in https://github.com/natemsz/evesynapse/pull/490
### 🐛 Bug Fixes
* Landing: restore the mockup's darkening, glass, and background treatment by @natemsz in https://github.com/natemsz/evesynapse/pull/489


**Full Changelog**: https://github.com/natemsz/evesynapse/compare/v0.5.3.5...v0.5.3.6

## [v0.5.3.5] - 2026-10-19

Automated build of EveSynapse v0.5.3.5. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).


## What's Changed
### 🚀 Features
* Option to fully revoke a character's token on unlink by @natemsz in https://github.com/natemsz/evesynapse/pull/484
* Landing restructure: trust and playstyles up top, alternating feature sections by @natemsz in https://github.com/natemsz/evesynapse/pull/485
* Add PKCE to the EVE SSO flow by @natemsz in https://github.com/natemsz/evesynapse/pull/486


**Full Changelog**: https://github.com/natemsz/evesynapse/compare/v0.5.3.4...v0.5.3.5

## [v0.5.3.4] - 2026-10-19

Automated build of EveSynapse v0.5.3.4. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).


## What's Changed
### 🚀 Features
* Appraisal: readable how-to-paste instructions instead of the fine-print block by @natemsz in https://github.com/natemsz/evesynapse/pull/478
* Landing backdrop re-encode, SVG wordmark, single-family footer by @natemsz in https://github.com/natemsz/evesynapse/pull/481
### 🐛 Bug Fixes
* Price global-market types (PLEX) from the global market book by @natemsz in https://github.com/natemsz/evesynapse/pull/479
* Price global-market types in fit valuation and the tradefinder by @natemsz in https://github.com/natemsz/evesynapse/pull/482
### 🧰 Maintenance & Dependencies
* Trim the CSP: no Cloudflare hosts, no nonce by @natemsz in https://github.com/natemsz/evesynapse/pull/480
* Regenerate sqlc output and guard it in CI by @natemsz in https://github.com/natemsz/evesynapse/pull/483


**Full Changelog**: https://github.com/natemsz/evesynapse/compare/v0.5.3.3...v0.5.3.4

## [v0.5.3.3] - 2026-10-19

Automated build of EveSynapse v0.5.3.3. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).


## What's Changed
### 🐛 Bug Fixes
* Fix empty hub console on appraisal share and error pages by @natemsz in https://github.com/natemsz/evesynapse/pull/477


**Full Changelog**: https://github.com/natemsz/evesynapse/compare/v0.5.3.2...v0.5.3.3

## [v0.5.3.2] - 2026-10-19

Automated build of EveSynapse v0.5.3.2. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).


## What's Changed
### 🚀 Features
* Contract search: matched-item pinning, sortable headers, location filters, sweep telemetry by @natemsz in https://github.com/natemsz/evesynapse/pull/473
* Appraisal page: split workbench layout by @natemsz in https://github.com/natemsz/evesynapse/pull/475
### 🧰 Maintenance & Dependencies
* Landing screenshots at display size by @natemsz in https://github.com/natemsz/evesynapse/pull/474
* Static cache: wallpaper URLs carry content-hash tokens (immutable caching) by @natemsz in https://github.com/natemsz/evesynapse/pull/476


**Full Changelog**: https://github.com/natemsz/evesynapse/compare/v0.5.3.1...v0.5.3.2

## [v0.5.3.1] - 2026-10-19

Automated build of EveSynapse v0.5.3.1. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).


## What's Changed
### 🚀 Features
* feat(appraisal): user-selectable pricing basis (hub, mode, percent) by @natemsz in https://github.com/natemsz/evesynapse/pull/458
* feat(appraisal): reprocessing value and volume by @natemsz in https://github.com/natemsz/evesynapse/pull/459
* feat(fitting): anonymous fit sharing by link by @natemsz in https://github.com/natemsz/evesynapse/pull/460
* feat(appraisal): moon-scan valuation by @natemsz in https://github.com/natemsz/evesynapse/pull/461
* feat(appraisal): exclusion list (list A minus list B) by @natemsz in https://github.com/natemsz/evesynapse/pull/462
* feat(appraisal): quantity multiplier by @natemsz in https://github.com/natemsz/evesynapse/pull/463
* feat(appraisal): multi-sell export format by @natemsz in https://github.com/natemsz/evesynapse/pull/465
* feat(appraisal): hub-comparison strip by @natemsz in https://github.com/natemsz/evesynapse/pull/466
* Appraisal parity batch (consolidated stack #457-#467) by @natemsz in https://github.com/natemsz/evesynapse/pull/471
### 🐛 Bug Fixes
* fix(appraisal): reject empty pastes, store anonymous appraisals, add delete and expiry by @natemsz in https://github.com/natemsz/evesynapse/pull/457
* Landing: live alerts, priced fit showcase, full public tools list, accessibility fixes by @natemsz in https://github.com/natemsz/evesynapse/pull/472


**Full Changelog**: https://github.com/natemsz/evesynapse/compare/v0.5.3.0...v0.5.3.1

## [v0.5.3.0] - 2026-10-19

Automated build of EveSynapse v0.5.3.0. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).


## What's Changed
### 🐛 Bug Fixes
* Balance chart: readable text at every placement by @natemsz in https://github.com/natemsz/evesynapse/pull/464
* Contract results: "And N more items" toggles the row drawer by @natemsz in https://github.com/natemsz/evesynapse/pull/468
* Roll position label: below-median rolls say Bottom, not Top by @natemsz in https://github.com/natemsz/evesynapse/pull/470
### 🧰 Maintenance & Dependencies
* docs(appraisal): state EFT, blueprint-copy and mutated-item appraisal plainly by @natemsz in https://github.com/natemsz/evesynapse/pull/456


**Full Changelog**: https://github.com/natemsz/evesynapse/compare/v0.5.2.14...v0.5.3.0

## [v0.5.2.14] - 2026-10-19

Automated build of EveSynapse v0.5.2.14. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).


## What's Changed
### 🐛 Bug Fixes
* Contract search polish: plainer labels, 5-line contents cap, type scale and chip fixes by @natemsz in https://github.com/natemsz/evesynapse/pull/451
* Restore the page-body top border on two-tier (grouped) nav pages by @natemsz in https://github.com/natemsz/evesynapse/pull/452
* Abyssal probe queue: mutable types only, newest first by @natemsz in https://github.com/natemsz/evesynapse/pull/453
### 🧰 Maintenance & Dependencies
* docs(changelog): backfill releases v0.5.2.10 through v0.5.2.12 by @natemsz in https://github.com/natemsz/evesynapse/pull/454
* Fix the changelog automation: it never fires for bot-published releases by @natemsz in https://github.com/natemsz/evesynapse/pull/455


**Full Changelog**: https://github.com/natemsz/evesynapse/compare/v0.5.2.13...v0.5.2.14

## [v0.5.2.13] - 2026-10-18

### Added
- Automated build of EveSynapse v0.5.2.13. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).

### Changed
- PR #450 — Contract search: results as the five-column table with inline appraisal/telemetry drawers.

### Fixed
- No changes recorded for this release window.

## [v0.5.2.12] - 2026-10-18

### Added
- Automated build of EveSynapse v0.5.2.12. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).

### Changed
- PR #448 — Swarm: signed pull auth (dual-accept) + public contracts as a swarm kind.
- PR #449 — Admin: peer Build column — release-library lookup of a peer's binary SHA claim.

## [v0.5.2.11] - 2026-10-18

### Added
- Automated build of EveSynapse v0.5.2.11. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).

### Changed
- PR #446 — Nav: rename corporation Orders to My Orders.
- PR #447 — Add release-notes category config (.github/release.yml).

## [v0.5.2.10] - 2026-10-18

### Added
- Automated build of EveSynapse v0.5.2.10. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).

## [v0.5.2.7] - 2026-10-18

### Added
- Automated build of EveSynapse v0.5.2.7. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).

### Changed
- PR #431 — Appraisal: price the wanted side of item exchanges.
- PR #430 — Contract search: drawer grid, title, colors, hunts, item icons, swap re-wiring, spread phrasing.

### Fixed
- Appraisal logic now prices the wanted side of item exchanges and gates deal ratio on full wanted coverage.
- Contract search UI fixes for drawers, row expansion, item icons, and spread phrasing.

## [v0.5.2.6] - 2026-10-18

### Added
- Automated build of EveSynapse v0.5.2.6. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).

### Changed
- PR #429 — Contract search structure pass.
- PR #428 — Contracts search: drop duplicate in-panel title, add section tabs.
- PR #427 — Home: header alignment and finishing, glance flush edges, dropdown menu treatment.

### Fixed
- Contract search structure and navigation updates.
- Home page header alignment, glance card spacing, and dropdown treatment.

## [v0.5.2.5] - 2026-10-18

### Added
- Automated build of EveSynapse v0.5.2.5. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).

### Changed
- PR #426 — Contracts: open surfaces, shared control styling, humanized attribute labels.
- PR #425 — bg-calm tiers re-derived from restored master.

### Fixed
- Contract surface styling and attribute label readability.
- Rebuilt background asset tiers for the calm interior theme.

## [v0.5.2.4] - 2026-10-18

### Added
- Automated build of EveSynapse v0.5.2.4. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).

### Changed
- PR #424 — Contract search redesign: filter tiers, rich rows, inline expansion.

### Fixed
- Contract search redesign and new inline expansion behavior.

## [v0.5.2.3] - 2026-10-18

### Added
- Automated build of EveSynapse v0.5.2.3. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).

### Changed
- PR #423 — Review fixes: briefing filters in the header row, separator under the fleet search row.

### Fixed
- Briefing filter placement and fleet search separator styling.

## [v0.5.2.2] - 2026-10-18

### Added
- Automated build of EveSynapse v0.5.2.2. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).

### Changed
- PR #422 — Home design pass: compact header, floating KPI band, solid warm glass, fleet header tags, calm interior sky.

### Fixed
- Home page visual treatment, glass styling, background, and fleet header improvements.

## [v0.5.2.1] - 2026-10-18

### Added
- Automated build of EveSynapse v0.5.2.1. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).

### Changed
- PR #421 — Argon audit remainder: record name search, leaderboard batching, home shared fetches, region-pills blur.
- PR #420 — Argon audit pickup 6: conditional page stylesheets, public-fits count fold.
- PR #419 — Mail: Inbox, Sent and All views.

### Fixed
- Search, home fetch, mail box behavior, and stylesheet loading fixes.

## [v0.5.2.0] - 2026-10-18

### Added
- Automated build of EveSynapse v0.5.2.0. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).

### Changed
- PR #418 — Bound fit quantities, fix module picker icons, normalize Home watchlist regions.
- PR #417 — Schema 059: the audit's missing indexes.
- PR #416 — Argon audit pickup 5: Discord re-claim clears previous owner's rules (1.8e).
- PR #414 — Pickup 3: N+1 fixes (audit 3.2) and page-wants sweep (3.3.2).
- PR #412 — Argon audit pickup 1: verified security, bug and performance fixes.
- PR #411 — Appraisal tool (/market/appraise/).
- PR #409 — Abyssal appraisal: scoring, stored estimates, contract surface (schema 057).
- PR #408 — Cleanup/remove home tree.
- PR #407 — Fitting: module row name shows the tooltip instead of removing; stepper in a fixed right-side cluster.
- PR #406 — Form controls: glass treatment for inputs, selects and textareas.
- PR #405 — Intel: Faction Warfare systems table gains a Region column, region-major order.
- PR #404 — Contract search: appraisal core, public search + detail, View in game.
- PR #403 — Structure page: persist context from name lookups.
- PR #402 — Fit card rim and divider; Corporation sidebar mirrors tab groups.

### Fixed
- Multiple UI, schema, appraisal, and performance fixes across the app.

## [v0.5.1.24] - 2026-10-17

### Added
- Automated build of EveSynapse v0.5.1.24. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).

### Changed
- No merged pull requests in the interval since v0.5.1.23.

### Fixed
- No changes recorded for this release window.

## [v0.5.1.23] - 2026-10-17

### Added
- Automated build of EveSynapse v0.5.1.23. Update with: evesynapse -update -arm64 (or -x86, -amd64, -x64 on Intel/AMD computers).

### Changed
- PR #399 — Notification settings: Save button right-aligned on its row.
- PR #397 — Home: briefing cards at mockup scale — big display titles (rebuilt onto current main).
- PR #396 — Notification settings: Channels and Types sections, one Discord switch, browser panel back at the bottom.
- PR #393 — Guide-average removal sweep: every remaining consumer on the house price order.
- PR #392 — Market: Fuzzwork fallback prices under our own sweeps.
- PR #391 — Glass: fleet summary strip transparent, glass tint a touch lighter, blur 42px.
- PR #389 — Split the Industry page into Blueprints, My Jobs and Mining Ledger.
- PR #390 — Market: hub region sweeps every 30 minutes, not hourly.
- PR #385 — Item browser: published-only filter that prunes the tree in place.
- PR #384 — Notifications: a first-run prompt under the bell, with channel switches.
- PR #383 — Fix: a hover prefetch no longer re-pins the acting character.
- PR #382 — Tabs: scroll-strip rail below 1313px + Corporation grouped into four clusters.

### Fixed
- UI polishing, data sweep logic, market fallback pricing, and page navigation refinements.
