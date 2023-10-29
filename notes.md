# Nofap bot
FriendlyBroccolli golang

# To-do
[x] Middleware message count
[x] Bot.SetCommands()
[x] config.yml
[x] README.md
[ ] Add inline keyboard (/new)
[ ] Finish getRank()
[ ] Remove replace in go.mod
[ ] Remove comment when issue fixed (i18n.SetDefaultLanguage("en-US"))
[x] Fix cauliflower and integrate
[ ] Remove db logger
[ ] /start
[x] /new
[x] /check
[x] /task
[ ] /motivation
[ ] /profile [@user=me]
[ ] /account
[x] /ranks
[x] /ranks [rank]
[-] /help (channels, donation)

# Structure
Commands:
- /start -> guided visit
- /new -> new journey (days, save to db, rank system, save to db)
- /check -> new entry (max 3/day, relapse?, note, text, public?, save to db)
- /task -> random task to complete (completed? -> save to db)
- /motivation -> random image or text
- /profile [@user=me] -> total score, current journey (start, rank, next rank, n. entries, n. tasks, score), all journeys (average length, total days, total entries), public entries (callback query button)
- /account -> score, rank, next rank, all entries, activity (new, check (id, note, relapse?), task), activity/journey, download
- /ranks -> ranks system overview
- /ranks [rank] -> full rank list
- /help -> command list, bot channel, personal channel, stats (users, uptime, messages count) contact, donation

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