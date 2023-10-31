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

	db.AutoMigrate(&User{}, &Journey{}, &Entry{}, &Task{}, &Motivation{})

	// load motivation images
	var motivations []Motivation
	var matches []string

	if err := filepath.Walk(config.MotivationPath, func(path string, file fs.FileInfo, err error) error {
		if err != nil {
			return err
		}

		if file.IsDir() {
			return nil
		}

		s := strings.Split(file.Name(), ".")

		if len(s) > 4 {
			matches = append(matches, s[0], s[2])
			motivationsCategories[s[2]] += 1

			place, err := strconv.Atoi(s[1])
			if err != nil {
				return err
			}

			motivations = append(motivations, Motivation{
				UUID:      randomString(16),
				Pack:      s[0],
				PackPlace: place,
				Category:  s[2],
				Language:  s[3],
				Extension: s[4],
				Path:      path,
			})

			return nil
		}

		matches = append(matches, s[0], s[1])
		motivationsCategories[s[1]] += 1

		motivations = append(motivations, Motivation{
			UUID:      randomString(16),
			ID:        s[0],
			Category:  s[1],
			Language:  s[2],
			Extension: s[3],
			Path:      path,
		})

		return nil
	}); err != nil {
		log.Fatalf("filepath: %v", err)
	}

	db.Create(&motivations)

	// initialize closest match
	cm = closestmatch.New(removeDuplicate(matches), []int{2})

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

	start = time.Now()
}

func main() {
	// make ranks buttons
	var ranksMarkup = b.NewMarkup()
	var ranksbuttons []telebot.Btn

	for _, rank := range config.Ranks {
		button := ranksMarkup.Data(rank.Name, randomString(16), rank.Name)
		ranksbuttons = append(ranksbuttons, button)

		b.Handle(&button, func(c telebot.Context) error {
			journey := &Journey{
				UserID: c.Sender().ID,
				End:    time.Time{},
			}

			db.Model(&journey).Where(&journey).Updates(map[string]interface{}{"rank_system": c.Callback().Data})

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

	// handle message count
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
		var found bool
		db.Raw("SELECT EXISTS(SELECT 1 FROM journeys WHERE user_id = ? AND end = ?) AS found", c.Sender().ID, time.Time{}).Scan(&found)
		if found {
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

		_, err = b.Edit(msg, text, ranksMarkup)
		return err
	})

	b.Handle("/check", func(c telebot.Context) error {
		var found bool
		db.Raw("SELECT EXISTS(SELECT 1 FROM journeys WHERE user_id = ? AND end = ?) AS found", c.Sender().ID, time.Time{}).Scan(&found)
		if !found {
			return c.Send(localizer.Tr(c.Sender().LanguageCode, "check-no-journey"))
		}

		now, midnight := today()

		var count int64
		db.Model(&Entry{}).Where("created_at BETWEEN ? AND ?", midnight, now).Count(&count)
		if int(count) >= 3 {
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
			markup := sliceMarkup(5, []string{"1", "2", "3", "4", "5", "6", "7", "8", "9", "10"})

			return c.Edit(localizer.Tr(c.Sender().LanguageCode, "survived-ask-note"), markup)
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
				IsPublic: false,
				Note: number,
				Text: answer.Text,
			})

			markup := b.NewMarkup()

			public := markup.Data(localizer.Tr(c.Sender().LanguageCode, "survived-button-public"), "public")
			private := markup.Data(localizer.Tr(c.Sender().LanguageCode, "survived-button-private"), "private")

			b.Handle(&public, func(c telebot.Context) error {
				return handlePrivacy(c, true)
			})

			b.Handle(&private, func(c telebot.Context) error {
				return handlePrivacy(c, false)
			})

			markup.Inline(markup.Row(public, private))

			_, err = b.Edit(msg, localizer.Tr(c.Sender().LanguageCode, "survived-ask-public"), markup)
			return err
		}

		return nil
	})

	b.Handle("/task", func(c telebot.Context) error {
		now, midnight := today()

		var count int64
		db.Model(&Task{}).Where("user_id = ? AND updated_at BETWEEN ? AND ?", c.Sender().ID, now, midnight).Count(&count)
		if int(count) >= 3 {
			return c.Send(localizer.Tr(c.Sender().LanguageCode, "task-too-much"))
		}

		var task Task

		if r := db.Model(&task).First(Task{
			UserID: c.Sender().ID,
			IsDone: false,
		}); r.RowsAffected > 0 {
			chat, err := b.ChatByID(task.ChatID)
			if err != nil {
				return err
			}

			message, err := strconv.Atoi(task.MessageID)
			if err != nil {
				return err
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
			button := markup.Data(localizer.Tr(c.Sender().LanguageCode, "task-button"), randomString(16))

			b.Handle(&button, func(c telebot.Context) error {
				var task Task

				if r := db.Last(&task, Task{
					UserID: c.Sender().ID,
					IsDone: false,
				}); r.RowsAffected > 0 {
					db.Model(&task).Where(&task).Updates(Task{IsDone: true})

					taskText := localizer.Tr(c.Sender().LanguageCode, config.Tasks[task.TaskID].Task)
					taskPoints := config.Tasks[task.TaskID].Points

					text := localizer.Tr(c.Sender().LanguageCode, "task-done", taskText, task.CreatedAt.Format("02 Jan 06 15:04"), task.UpdatedAt.Format("02 Jan 06 15:04"), taskPoints)

					return c.Edit(text)
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
		if len(c.Args()) == 0 {
			var m Motivation
			db.Order("RANDOM()").Take(&m)

			if m.Pack != "" {
				return sendPack(c, m)
			}

			return c.Send(&telebot.Photo{
				File:    telebot.FromDisk(m.Path),
				Caption: localizer.Tr(c.Sender().LanguageCode, "motivation-caption", m.ID, m.Category, m.Language),
			})
		}

		arg := c.Args()[0]

		if arg == "list" {
			return c.Send(localizer.Tr(c.Sender().LanguageCode, "motivation-list", motivationsCategories))
		}

		c.Notify(telebot.UploadingPhoto)

		var m Motivation

		if r := db.Where("pack = ? OR id = ? OR category = ?", arg, arg, arg).Order("RANDOM()").Take(&m); r.RowsAffected == 0 {
			return c.Send(localizer.Tr(c.Sender().LanguageCode, "motivation-error", cm.Closest(arg)))
		}

		if m.Pack != "" {
			return sendPack(c, m)
		}

		return c.Send(&telebot.Photo{
			File:    telebot.FromDisk(m.Path),
			Caption: localizer.Tr(c.Sender().LanguageCode, "motivation-caption", m.ID, m.Category, m.Language),
		})
	})

	b.Handle("/profile", func(c telebot.Context) error {
		var user User

		if len(c.Args()) > 0 {
			username := strings.Trim(c.Args()[0], "@")

			if r := db.Last(&user, "username = ?", username); errors.Is(r.Error, gorm.ErrRecordNotFound) {
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
		var users int64
		db.Model(&Journey{}).Distinct("user_id").Count(&users)

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
	db.Select("start").Find(&a, &Journey{
		UserID: user.ID,
	})

	var totalDays int
	for _, j := range a {
		totalDays += int(time.Now().Sub(j.Start).Hours() / 24)
	}

	averageDays := totalDays

	if totalDays > 0 && len(a) > 0 {
		averageDays = totalDays / len(a)
	}

	var entriesCount, tasksCount, totalEntriesCount, totalTasksCount int64
	
	db.Model(&Entry{}).Where("user_id = ? AND created_at > ?", user.ID, j.Start).Count(&entriesCount)
	db.Model(&Task{}).Where("user_id = ? AND created_at > ?", user.ID, j.Start).Count(&tasksCount)
	db.Model(&Entry{}).Where("user_id = ?", user.ID).Count(&totalEntriesCount)
	db.Model(&Task{}).Where("user_id = ?", user.ID).Count(&totalTasksCount)

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

	return c.EditOrSend(text, markup)
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

	var count int64
	db.Model(&Entry{
		UserID: user.ID,
		IsPublic: true,
	}).Count(&count)

	var entries []Entry
	db.Limit(10).Offset((page - 1) * 10).Find(&entries, Entry{
		UserID: user.ID,
		IsPublic: true,
	})

	text := localizer.Tr(c.Sender().LanguageCode, "profile-entries", map[string]interface{}{
		"User": user.Username,
		"Page": page,
		"MaxPage": count / 10 + 1,
		"Entries": entries,
	})

	markup := b.NewMarkup()

	var previous, next telebot.Btn

	if page > 1 {
		previous = markup.Data(localizer.Tr(c.Sender().LanguageCode, "profile-previous"), randomString(16), strconv.FormatInt(user.ID, 10), strconv.Itoa(page - 1))
	}

	if int(count / 10) > page {
		next = markup.Data(localizer.Tr(c.Sender().LanguageCode, "profile-next"), randomString(16), strconv.FormatInt(user.ID, 10), strconv.Itoa(page + 1))
	}

	back := markup.Data(localizer.Tr(c.Sender().LanguageCode, "profile-back"), randomString(16), strconv.FormatInt(user.ID, 10))

	markup.Inline(markup.Row(previous, next, back))

	b.Handle(&next, profilePublicEntries)
	b.Handle(&previous, profilePublicEntries)
	b.Handle(&back, func(c telebot.Context) error {
		id, err := strconv.ParseInt(c.Callback().Data, 10, 64)
		if err != nil {
			return err
		}

		var user User
		db.FirstOrCreate(&user, User{ID: id})
		return profile(c, user)
	})

	return c.Edit(text, markup)
}

func handlePrivacy(c telebot.Context, isPublic bool) error {
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

	db.Model(&entry).Where(&entry).Updates(Entry{IsPublic: isPublic})

	return c.Edit(localizer.Tr(c.Sender().LanguageCode, "survived-saved", privacy, entry.Note, command, entry.Text))
}

func sendPack(c telebot.Context, m Motivation) error {
	var p []Motivation
	db.Where("pack = ?", m.Pack).Find(&p)

	sort.SliceStable(p, func(i, j int) bool {
		return p[i].PackPlace < p[j].PackPlace
	})

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

	return c.Send(localizer.Tr(c.Sender().LanguageCode, "motivation-caption", m.Pack, m.Category, m.Language))
}

func getRank(start time.Time, rank string, offset int) (int, string) {
	days := int(time.Now().Sub(start).Hours() / 24)
	levels := config.Ranks[strings.ToLower(rank)].Levels

	keys := maps.Keys(levels)
	sort.Ints(keys)

	for key, value := range keys {
		if days <= value {
			return value, levels[keys[key+offset]]
		}
	}

	return 0, ""
}

func sliceMarkup(split int, data []string) *telebot.ReplyMarkup {
	var buttons []telebot.Btn
	markup := b.NewMarkup()

	for _, text := range data {
		button := markup.Data(text, text)
		buttons = append(buttons, button)
	}

	markup.Inline(markup.Split(split, buttons)...)

	return markup
}

func randomString(n int) string {
	var letters = []rune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789")

	s := make([]rune, n)
	for i := range s {
		s[i] = letters[rand.Intn(len(letters))]
	}
	return string(s)
}

func calculateScore(userID int64, allJourneys bool) int {
	score := 0

	var tasks []Task

	if allJourneys {
		var journeys []Journey
		db.Select("start", "end").Where("user_id = ?", userID).Find(&journeys)

		for _, journey := range journeys {
			end := journey.End
			if end.IsZero() {
				end = time.Now()
			}
			score += int(end.Sub(journey.Start).Hours() / 24) * 2
		}

		db.Select("task_id").Where("user_id = ?", userID).Find(&tasks)
	} else {
		var journey Journey
		db.Select("start").Where("user_id = ? AND end = ?", userID, time.Time{}).Last(&journey)

		if !journey.Start.IsZero() {
			score += int(time.Now().Sub(journey.Start).Hours() / 24) * 2
			db.Select("task_id").Where("? < updated_at AND user_id = ?", journey.Start, userID).Find(&tasks)
		}

	}
	
	for _, task := range tasks {
		score += config.Tasks[task.TaskID].Points
	}

	return score
}

func today() (time.Time, time.Time) {
	now := time.Now()
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())

	return now, midnight
}

func removeDuplicate[T string | int](sliceList []T) []T {
    allKeys := make(map[T]bool)
    list := []T{}
    for _, item := range sliceList {
        if _, value := allKeys[item]; !value {
            allKeys[item] = true
            list = append(list, item)
        }
    }
    return list
}