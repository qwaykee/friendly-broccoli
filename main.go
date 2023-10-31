package main

import (
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/kataras/i18n"
	"github.com/qwaykee/cauliflower"
	"github.com/schollz/closestmatch"
	"gopkg.in/telebot.v3"
	"gopkg.in/yaml.v3"

	"embed"
	"golang.org/x/exp/maps"
	"io/fs"
	"log"
	"math/rand"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"errors"
)

var (
	config    Config
	localizer *i18n.I18n
	db        *gorm.DB
	b         *telebot.Bot
	i         *cauliflower.Instance
	cm        *closestmatch.ClosestMatch

	messageCount          int
	chatToChannel         = make(map[int64](*chan string))
	rankButtons           = []telebot.Btn{}
	motivationsCategories = make(map[string]int)
	motivationsPacks      = make(map[string][]Motivation)
	start                 time.Time

	//go:embed locale.*.yml
	localesFS embed.FS

	//go:embed config.yml
	configFile []byte
)

func init() {
	// load config
	if err := yaml.Unmarshal(configFile, &config); err != nil {
		log.Fatal(err)
	}

	// load motivation images
	config.Motivations = make(map[string]Motivation)
	if err := filepath.Walk(config.MotivationPath, func(path string, file fs.FileInfo, err error) error {
		if err != nil {
			return err
		}

		if !file.IsDir() {
			s := strings.Split(file.Name(), ".")
			var pack, place, id, category, language, extension string
			var m Motivation
			if len(s) > 4 {
				pack, place, category, language, extension = s[0], s[1], s[2], s[3], s[4]
				placeInt, err := strconv.Atoi(place)
				if err != nil {
					return err
				}

				m = Motivation{
					Pack:      pack,
					PackPlace: placeInt,
					Category:  category,
					Language:  language,
					Extension: extension,
					Path:      path,
				}

				motivationsPacks[pack] = append(motivationsPacks[pack], m)
			} else {
				id, category, language, extension = s[0], s[1], s[2], s[3]

				m = Motivation{
					ID:        id,
					Category:  category,
					Language:  language,
					Extension: extension,
					Path:      path,
				}
			}

			motivationsCategories[category] += 1

			config.Motivations[id] = m
		}

		return nil
	}); err != nil {
		log.Fatalf("filepath: %v", err)
	}

	for _, pack := range motivationsPacks {
		sort.Slice(pack, func(i, j int) bool {
			return pack[i].PackPlace < pack[j].PackPlace
		})
	}

	// initialize i18n
	loader, err := i18n.FS(localesFS, "locale.*.yml")
	if err != nil {
		log.Fatalf("loader: %v", err)
	}

	localizer, err = i18n.New(loader, "en-US", "fr-FR")
	if err != nil {
		log.Fatalf("localizer: %v", err)
	}

	// initialize database
	db, err = gorm.Open(sqlite.Open(config.Database), &gorm.Config{PrepareStmt: true})
	if err != nil {
		log.Fatalf("gorm: %v", err)
	}

	db.AutoMigrate(&User{}, &Journey{}, &Entry{}, &Task{})

	// create bot and set commands
	b, err = telebot.NewBot(telebot.Settings{
		Token:     config.Token,
		Poller:    &telebot.LongPoller{Timeout: time.Duration(config.Timeout) * time.Second},
		ParseMode: telebot.ModeMarkdown,
	})
	if err != nil {
		log.Fatalf("telebot: %v", err)
	}

	if config.SetCommands {
		var c []telebot.Command
		for name, description := range config.Commands {
			c = append(c, telebot.Command{
				Text:        name,
				Description: description,
			})
		}
		if err := b.SetCommands(c); err != nil {
			log.Fatalf("telebot: %v", err)
		}
	}

	// initialize cauliflower
	i, err = cauliflower.NewInstance(cauliflower.Settings{
		Bot:    b,
		Cancel: "/cancel",
		TimeoutHandler: func(c telebot.Context) error {
			return c.Send(localizer.Tr(c.Sender().LanguageCode, "err-no-message-received"))
		},
		CancelHandler: func(c telebot.Context) error {
			return c.Send(localizer.Tr(c.Sender().LanguageCode, "err-command-canceled"))
		},
		InstallMiddleware: true,
	})
	if err != nil {
		log.Fatalf("cauliflower: %v", err)
	}

	// initialize closest match
	matches := append(maps.Keys(config.Motivations), maps.Keys(motivationsCategories)...)
	matches = append(matches, maps.Keys(motivationsPacks)...)
	cm = closestmatch.New(matches, []int{2})

	start = time.Now()
}

func main() {
	// make ranks buttons
	var ranksMarkup telebot.ReplyMarkup
	var ranksbuttons []telebot.Btn

	for _, rank := range config.Ranks {
		button := ranksMarkup.Data(rank.Name, randomString(16), rank.Name)
		ranksbuttons = append(ranksbuttons, button)

		b.Handle(&button, func(c telebot.Context) error {
			journey := &Journey{
				UserID: c.Sender().ID,
				End:    time.Time{},
			}

			db.Last(&journey)

			journey.RankSystem = c.Callback().Data

			db.Save(&journey)

			_, rank := getRank(journey.Start, journey.RankSystem, 0)

			text := localizer.Tr(
				c.Sender().LanguageCode,
				"new-saved",
				rank,
				journey.RankSystem,
				journey.Start.Format("02 Jan 06"),
				int(time.Now().Sub(journey.Start).Hours()/24),
			)

			return c.Edit(text)
		})
	}

	ranksMarkup.Inline(ranksMarkup.Split(2, ranksbuttons)...)

	// handle message count and save user
	b.Use(func(next telebot.HandlerFunc) telebot.HandlerFunc {
		return func(c telebot.Context) error {
			messageCount += 1
			return next(c)
		}
	})

	b.Handle("/start", func(c telebot.Context) error {
		db.Save(&User{
			ID: c.Sender().ID,
			Username: c.Sender().Username,
		})

		return c.Send(localizer.Tr(c.Sender().LanguageCode, "start-hello"))
	})

	b.Handle("/new", func(c telebot.Context) error {
		if r := db.Where("user_id = ? AND end = ?", c.Sender().ID, time.Time{}).First(&Journey{}); !errors.Is(r.Error, gorm.ErrRecordNotFound) {
			return c.Send(localizer.Tr(c.Sender().LanguageCode, "new-already-running-journey"))
		}

		msg, answer, err := i.Listen(cauliflower.Parameters{
			Context: c,
			Message: localizer.Tr(c.Sender().LanguageCode, "new-ask-streak"),
		})
		if err != nil {
			return nil
		}

		days, err := strconv.Atoi(answer.Text)
		if err != nil {
			return c.Send(localizer.Tr(c.Sender().LanguageCode, "new-not-a-number"))
		}

		start := time.Now().Add(-time.Duration(days) * time.Hour * 24)
		text := localizer.Tr(c.Sender().LanguageCode, "new-ask-rank", start.Format("02 Jan 06"))

		db.Create(&Journey{
			UserID: c.Sender().ID,
			Start:  start,
		})

		_, err = b.Edit(msg, text, &ranksMarkup)
		return err
	})

	b.Handle("/check", func(c telebot.Context) error {
		if r := db.Where("user_id = ? AND end = ?", c.Sender().ID, time.Time{}).Last(&Journey{}); errors.Is(r.Error, gorm.ErrRecordNotFound) {
			return c.Send(localizer.Tr(c.Sender().LanguageCode, "check-no-journey"))
		}

		now := time.Now()
		midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())

		if r := db.Find(&Entry{}).Where("created_at BETWEEN ? AND ?", midnight, now); r.RowsAffected >= 3 {
			return c.Send(localizer.Tr(c.Sender().LanguageCode, "check-already-checked-in"))
		}

		markup := b.NewMarkup()

		relapsed := markup.Data(localizer.Tr(c.Sender().LanguageCode, "check-button-relapsed"), "relapsed")
		survived := markup.Data(localizer.Tr(c.Sender().LanguageCode, "check-button-survived"), "survived")

		b.Handle(&relapsed, func(c telebot.Context) error {
			msg, answer, err := i.Listen(cauliflower.Parameters{
				Context: c,
				Message: localizer.Tr(c.Sender().LanguageCode, "relapsed"),
				Edit: c.Message(),
			})
			if err != nil {
				return nil
			}

			db.Where("user_id = ?", c.Sender().ID).Updates(&Journey{
				End:    time.Now(),
				Text:   answer.Text,
			})

			_, err = b.Edit(msg, localizer.Tr(c.Sender().LanguageCode, "relapsed-saved"))
			return err
		})

		b.Handle(&survived, func(c telebot.Context) error {
			noteButtons := sliceMarkup(5, []string{"1", "2", "3", "4", "5", "6", "7", "8", "9", "10"})

			return c.Edit(localizer.Tr(c.Sender().LanguageCode, "survived-ask-note"), noteButtons)
		})

		markup.Inline(markup.Row(relapsed, survived))

		return c.Send(localizer.Tr(c.Sender().LanguageCode, "check-ask-relapsed"), markup)
	})

	b.Handle(telebot.OnCallback, func(c telebot.Context) error {
		data := strings.TrimSpace(c.Callback().Data)

		if number, err := strconv.Atoi(data); err == nil {
			// handle note from check
			msg, answer, err := i.Listen(cauliflower.Parameters{
				Context: c,
				Message: localizer.Tr(c.Sender().LanguageCode, "survived-ask-entry"),
				Edit: c.Message(),
			})
			if err != nil {
				return nil
			}

			db.Create(&Entry{
				UserID: c.Sender().ID,
				CreatedAtStr: time.Now().Format("02 Jan 06 15:04"),
				Note:   number,
				Text:   answer.Text,
			})

			privacyButtons := b.NewMarkup()

			public := privacyButtons.Data(localizer.Tr(c.Sender().LanguageCode, "survived-button-public"), "public")
			private := privacyButtons.Data(localizer.Tr(c.Sender().LanguageCode, "survived-button-private"), "private")

			handlePrivacy := func(c telebot.Context, isPublic bool) error {
				var privacy, command string

				if isPublic {
					privacy = localizer.Tr(c.Sender().LanguageCode, "survived-public")
					command = "/profile"
				} else {
					privacy = localizer.Tr(c.Sender().LanguageCode, "survived-private")
					command = "/account"
				}

				entry := &Entry{
					UserID: c.Sender().ID,
					Note:   number,
					Text:   answer.Text,
				}

				db.Last(&entry)

				entry.IsPublic = true

				db.Save(&entry)

				return c.Edit(localizer.Tr(c.Sender().LanguageCode, "survived-saved", privacy, entry.Note, command, entry.Text))
			}

			b.Handle(&public, func(c telebot.Context) error {
				return handlePrivacy(c, true)
			})

			b.Handle(&private, func(c telebot.Context) error {
				return handlePrivacy(c, false)
			})

			privacyButtons.Inline(privacyButtons.Row(public, private))

			_, err = b.Edit(msg, localizer.Tr(c.Sender().LanguageCode, "survived-ask-public"), privacyButtons)
			return err
		}

		return nil
	})

	b.Handle("/task", func(c telebot.Context) error {
		var task Task

		now := time.Now()
		midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())

		if r := db.Find(&Task{}, "user_id = ? AND updated_at BETWEEN ? AND ?", c.Sender().ID, now, midnight); r.RowsAffected >= 3 {
			return c.Send(localizer.Tr(c.Sender().LanguageCode, "task-too-much"))
		}

		if r := db.Find(&task, Task{
			UserID: c.Sender().ID,
			IsDone: false,
		}); r.RowsAffected > 0 {
			chat, err := b.ChatByID(task.ChatID)
			if err != nil {
				log.Println(err)
			}

			message, err := strconv.Atoi(task.MessageID)
			if err != nil {
				log.Println(err)
			}

			msg := telebot.Message{
				ID:   message,
				Chat: chat,
			}

			_, err = b.Reply(&msg, localizer.Tr(c.Sender().LanguageCode, "task-unfinished"))
			return err
		} else {
			taskID := rand.Intn(len(config.Tasks))
			taskText := localizer.Tr(c.Sender().LanguageCode, config.Tasks[taskID].Task)

			text := localizer.Tr(c.Sender().LanguageCode, "task-cta", taskText, time.Now().Format("02 Jan 06 15:04"))

			markup := b.NewMarkup()
			buttonText := localizer.Tr(c.Sender().LanguageCode, "task-button")
			button := markup.Data(buttonText, randomString(16))

			b.Handle(&button, func(c telebot.Context) error {
				var task Task

				if r := db.Last(&task, Task{
					UserID: c.Sender().ID,
					IsDone: false,
				}); r.RowsAffected > 0 {
					task.IsDone = true

					db.Save(&task)

					taskText := localizer.Tr(c.Sender().LanguageCode, config.Tasks[task.TaskID].Task)
					taskPoints := config.Tasks[task.TaskID].Points

					text := localizer.Tr(c.Sender().LanguageCode, "task-done", taskText, task.CreatedAt.Format("02 Jan 06 15:04"), task.UpdatedAt.Format("02 Jan 06 15:04"), taskPoints)

					return c.Edit(c.Message(), text)
				}

				return nil
			})

			markup.Inline(markup.Row(button))

			msg, err := b.Send(c.Chat(), text, markup)
			if err != nil {
				log.Println(err)
			}

			db.Create(&Task{
				UserID:    c.Sender().ID,
				ChatID:    c.Chat().ID,
				MessageID: strconv.Itoa(msg.ID),
				TaskID:    taskID,
				IsDone:    false,
			})
		}
		return nil
	})

	b.Handle("/motivation", func(c telebot.Context) error {
		motivations := make(map[string]Motivation)

		if len(c.Args()) > 0 {
			arg := c.Args()[0]

			if arg == "list" {
				return c.Send(localizer.Tr(c.Sender().LanguageCode, "motivation-list", motivationsCategories))
			}

			if err := c.Notify(telebot.UploadingPhoto); err != nil {
				return err
			}

			if _, ok := motivationsCategories[arg]; ok {
				for _, m := range config.Motivations {
					if m.Category == arg {
						motivations[m.ID] = m
					}
				}
			} else if m, ok := config.Motivations[arg]; ok {
				return c.Send(&telebot.Photo{
					File:    telebot.FromDisk(m.Path),
					Caption: localizer.Tr(c.Sender().LanguageCode, "motivation-caption", m.ID, m.Category, m.Language),
				})
			} else if p, ok := motivationsPacks[arg]; ok {
				var album telebot.Album

				for _, image := range p {
					album = append(album, &telebot.Photo{File: telebot.FromDisk(image.Path)})
					if len(album) == 10 {
						c.SendAlbum(album)
						album = telebot.Album{}
					}
				}

				if len(album) > 0 {
					c.SendAlbum(album)
				}

				return c.Send(localizer.Tr(c.Sender().LanguageCode, "motivation-caption", p[0].Pack, p[0].Category, p[0].Language))
			} else {
				return c.Send(localizer.Tr(c.Sender().LanguageCode, "motivation-error", cm.Closest(arg)))
			}
		} else {
			motivations = config.Motivations
		}

		motivationsKeys := maps.Keys(motivations)
		m := motivations[motivationsKeys[rand.Intn(len(motivations))]]

		if m.Pack != "" {
			var album telebot.Album

			for _, image := range motivationsPacks[m.Pack] {
				album = append(album, &telebot.Photo{File: telebot.FromDisk(image.Path)})
				if len(album) == 10 {
					c.SendAlbum(album)
					album = telebot.Album{}
				}
			}

			if len(album) > 0 {
				c.SendAlbum(album)
			}

			return c.Send(localizer.Tr(c.Sender().LanguageCode, "motivation-caption", m.Pack, m.Category, m.Language))
		}

		return c.Send(&telebot.Photo{
			File:    telebot.FromDisk(m.Path),
			Caption: localizer.Tr(c.Sender().LanguageCode, "motivation-caption", m.ID, m.Category, m.Language),
		})
	})

	b.Handle("/profile", func(c telebot.Context) error {
		var user User

		if len(c.Args()) > 0 {
			if r := db.Last(&user, "username = ?", c.Args()[0]); errors.Is(r.Error, gorm.ErrRecordNotFound) {
				// user doesn't exist
				return c.Send(localizer.Tr(c.Sender().LanguageCode, "profile-text-no-journey"))
			}
		} else {
			user.ID, user.Username = c.Sender().ID, c.Sender().Username
		}

		return profile(c, user)
	})

	b.Handle("/ranks", func(c telebot.Context) error {
		var text string

		if len(c.Args()) > 0 {
			rank := config.Ranks[c.Args()[0]]

			text += "*" + rank.Name + "*\n"

			sortedLevels := maps.Keys(rank.Levels)
			sort.Ints(sortedLevels)

			for _, level := range sortedLevels {
				text += strconv.Itoa(level) + ": " + rank.Levels[level] + "\n"
			}
		} else {
			for _, rank := range config.Ranks {
				text += "*" + rank.Name + "*\n"

				sortedLevels := maps.Keys(rank.Levels)
				sort.Ints(sortedLevels)

				for _, level := range sortedLevels[:3] {
					text += strconv.Itoa(level) + ": " + rank.Levels[level] + "\n"
				}

				text += "...\n\n"
			}
		}

		return c.Send(text)
	})

	b.Handle("/help", func(c telebot.Context) error {
		users := db.Select("DISTINCT(user_id)").Find(&Journey{}).RowsAffected

		return c.Send(localizer.Tr(c.Sender().LanguageCode, "help-text", users, messageCount, start.Format("02 Jan 06 15:04")))
	})

	b.Handle("/dummy", func(c telebot.Context) error {
		db.Create(&User{
			ID: c.Sender().ID,
			Username: c.Sender().Username,
		})

		db.Create(&Journey{
			UserID: c.Sender().ID,
			RankSystem: "memes",
			Start: time.Now(),
		})

		db.Create(&Entry{
			UserID: c.Sender().ID,
			CreatedAtStr: time.Now().Format("02 Jan 06 15:04"),
			IsPublic: true,
			Note: 7,
			Text: "lzihfhlfih",
		})

		return c.Send("done")
	})

	log.Println("starting bot")
	b.Start()
}

func profile(c telebot.Context, user User) error {
	var j Journey
	if r := db.First(&j, "user_id = ?", user.ID); errors.Is(r.Error, gorm.ErrRecordNotFound) {
		// user doesn't have journeys
		return c.Send(localizer.Tr(c.Sender().LanguageCode, "profile-text-no-journey"))
	}

	var translationKey string
	if j.End.IsZero() {
		translationKey = "profile-current-journey"
	} else {
		translationKey = "profile-last-journey"
	}

	journeyIsCurrent := localizer.Tr(c.Sender().LanguageCode, translationKey)

	var a []Journey
	db.Find(&a, &Journey{
		UserID: user.ID,
	})

	var totalDays int
	for _, j := range a {
		totalDays += int(time.Now().Sub(j.Start).Hours() / 24)
	}

	averageDays := totalDays

	if totalDays > 0 {
		// to avoid division by zero
		averageDays = totalDays / len(a)
	}

	var tasks []Task
	var entries []Entry

	var totalEntriesCount, totalTasksCount int64
	
	totalEntriesCount = db.Where("user_id = ?", user.ID).Select("created_at").Find(&entries).RowsAffected
	totalTasksCount = db.Where("user_id = ?", user.ID).Select("created_at").Find(&tasks).RowsAffected

	var entriesCount, tasksCount int
	
	for _, entry := range entries {
		if entry.CreatedAt.After(j.Start) {
			entriesCount += 1
		}
	}
	
	for _, task := range tasks {
		if task.CreatedAt.After(j.Start) {
			tasksCount += 1
		}
	}

	_, currentRank := getRank(j.Start, j.RankSystem, 0)
	daysLeft, nextRank := getRank(j.Start, j.RankSystem, 1)

	days := int(time.Now().Sub(j.Start).Hours() / 24)
	totalScore, currentScore := calculateScore(user.ID, true), calculateScore(user.ID, false)

	text := localizer.Tr(c.Sender().LanguageCode,
		"profile-text",
		user.Username, // current/last journey
		totalScore,
		journeyIsCurrent,
		currentScore,
		j.Start.Format("02 Jan 06"),
		days,
		currentRank,
		nextRank,
		daysLeft,
		entriesCount,
		tasksCount,
		len(a), // all journeys
		averageDays,
		totalDays,
		totalEntriesCount,
		totalTasksCount,
	)

	markup := b.NewMarkup()

	button := markup.Data(localizer.Tr(c.Sender().LanguageCode, "profile-button", user.Username), randomString(16), strconv.FormatInt(user.ID, 10), "1")

	b.Handle(&button, profilePublicEntries)

	markup.Inline(markup.Row(button))

	return c.Send(text, markup)
}

func profilePublicEntries(c telebot.Context) error {
	data := strings.Split(c.Callback().Data, "|")

	var user User
	if r := db.First(&user, data[0]); errors.Is(r.Error, gorm.ErrRecordNotFound) {
		return c.Send(localizer.Tr(c.Sender().LanguageCode, "err-button"))
	}

	page, err := strconv.Atoi(data[1])
	if err != nil {
		return c.Send(localizer.Tr(c.Sender().LanguageCode, "err-button"))
	}

	var entries []Entry
	db.Limit(10).Offset((page - 1) * 10).Find(&entries, Entry{
		UserID: user.ID,
		IsPublic: true,
	})

	text := localizer.Tr(c.Sender().LanguageCode, "profile-entries", map[string]interface{}{
		"User": user.Username,
		"Page": page,
		"Entries": entries,
	})

	markup := b.NewMarkup()

	next := markup.Data(localizer.Tr(c.Sender().LanguageCode, "next"), randomString(16), strconv.FormatInt(user.ID, 10), strconv.Itoa(page + 1))
	previous := markup.Data(localizer.Tr(c.Sender().LanguageCode, "previous"), randomString(16), strconv.FormatInt(user.ID, 10), strconv.Itoa(page - 1))
	back := markup.Data(localizer.Tr(c.Sender().LanguageCode, "back"), randomString(16), strconv.FormatInt(user.ID, 10))

	markup.Inline(markup.Row(next, previous, back))

	b.Handle(&next, profilePublicEntries)
	b.Handle(&previous, profilePublicEntries)
	b.Handle(&back, func(c telebot.Context) error {
		var user User
		if r := db.Last(&user, "username = ?", c.Callback().Data); errors.Is(r.Error, gorm.ErrRecordNotFound) {
			// user doesn't exist
			return c.Send(localizer.Tr(c.Sender().LanguageCode, "profile-text-no-journey"))
		} else {
			return profile(c, user)
		}
	})

	return c.Edit(text, markup)
}