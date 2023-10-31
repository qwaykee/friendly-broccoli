# Nofap bot
FriendlyBroccolli golang

# To-do
[x] Middleware message count
[x] Bot.SetCommands()
[x] config.yml
[x] Add inline keyboard (/new)
[x] Finish getRank()
[x] Remove replace in go.mod
[x] Remove comment when issue fixed (i18n.SetDefaultLanguage("en-US")) // removed line
[x] Fix cauliflower and integrate
[ ] /start
[x] /new
[x] /check
[x] /task
[x] /motivation
[x] /profile [@user=me]
[ ] /account
[x] /ranks
[x] /ranks [rank]
[-] /help (channels, donation)
[x] Fix profile and /check relapsed update db doesn't work
[x] Fix profile public entries markup
[x] Task max 3/day
[x] Change /motivations structs to db
[ ] README.md
[ ] Remove db logger
[ ] Add tasks to config.yml
[ ] Add images to motivation/
[ ] Remove /dummy
[ ] Add resources to /help (easypeasymethod)
[ ] Finir /account entries, activity
[ ] /account download edit message instead of sending new one

# Structure
Commands:
- /start -> guided visit
- /new -> new journey (days, save to db, rank system, save to db)
- /check -> new entry (max 3/day, relapse?, note, text, public?, save to db)
- /task -> random task to complete (max 3/day, completed? -> save to db)
- /motivation -> random image
- /motivation list -> list categories
- /motivation [id] -> image id
- /motivation [category] -> random image from category
- /profile [@user=me] -> total score, current journey (start, days, rank, next rank, n. entries, n. tasks, score), all journeys (average length, total days, total entries), public entries (callback query button)
- /account -> score, rank, next rank, all entries, activity (new, check (id, note, relapse?), task), activity/journey, download
- /ranks -> ranks system overview
- /ranks [rank] -> full rank list
- /help -> command list, bot channel, personal channel, stats (users, uptime, messages count) contact, donation

Motivation filename:
- pack.packplace.category.languagecode.extension
- id.category.languagecode.extension
- id/pack must be unique
- category must not be equal to "list"

Score system:
- 2 points/day
- 2-10 points/task (3 task max/day)

No check-in:
- 3 days, remember
- 6 days, warn expiration
- 7 days, expire journey

Config:
- Token (str)
- Poller timeout (seconds)
- Set commands? -> True only once
- Commands
- Ranks (name str: name str, score int - levels: days int, level str)
- Tasks (points int: text str)

Database:
Journey:
- ID (pk) - int
- User id - int
- Rank system - int
- Start - time.Time
- End - time.Time
- Text - str
Entry:
- ID (pk) - int
- Journey id (foreign key) - int
- Date (autodate) - time.Time
- Is public? - bool
- Note - int
- Text - str
Task:
- ID (pk) - int
- Journey id (foreign key) - int
- Date (autodate) - time.Time
- Task id

# Metadata

Commands:
start - Start the bot
new - Start a new journey
check - Check-in for your current journey
motivation - Send a motivational media
task - Send a task to achieve
ranks - List the ranks systems
profile - See your public profile
account - See your private informations and settings
help - See commands help, statistics and bot channel