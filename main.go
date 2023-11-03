package main

import (
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/kataras/i18n"
	"github.com/qwaykee/cauliflower"
	"github.com/schollz/closestmatch"
	"gopkg.in/telebot.v3"
	"gopkg.in/telebot.v3/middleware"
	"gopkg.in/yaml.v3"

	"bytes"
	"embed"
	"errors"
	"golang.org/x/exp/maps"
	"io/fs"
	"log"
	"math/rand"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

var (
	config    Config
	localizer *i18n.I18n
	db        *gorm.DB
	b         *telebot.Bot
	i         *cauliflower.Instance
	cm        *closestmatch.ClosestMatch

	messageCount          int
	responseTime          []time.Duration
	rankButtons           = []telebot.Btn{}
	motivationsCategories = make(map[string]int)
	start                 time.Time

	//go:embed locale.*.yml
	localesFS embed.FS

	//go:embed config.yml
	configFile []byte
)

func init() {
	log.Println("initialization")

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

	db.AutoMigrate(&User{}, &Journey{}, &Entry{}, &Task{}, &Motivation{}, &TaskData{})

	// load motivation images into db and closest matches
	if err := update(); err != nil {
		log.Fatalf("updater motivation: %v", err)
	}

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
	// handle message count
	b.Use(func(next telebot.HandlerFunc) telebot.HandlerFunc {
		return func(c telebot.Context) error {
			start := time.Now()

			messageCount += 1
			err := next(c)

			responseTime = append(responseTime, time.Now().Sub(start))

			return err
		}
	})

	b.Handle("/start", commandStart)
	b.Handle("/new", commandNew)
	b.Handle("/check", commandCheck)
	b.Handle("/task", commandTask)
	b.Handle("/motivation", commandMotivation)
	b.Handle("/profile", commandProfile)
	b.Handle("/account", commandAccount)
	b.Handle("/ranks", commandRanks)
	b.Handle("/help", commandHelp)
	b.Handle("/fix", commandFix)

	admin := b.Group()

	admin.Use(middleware.Whitelist(config.Owners...))

	admin.Handle("/update", func(c telebot.Context) error {
		if err := update(); err != nil {
			return err
		}

		return c.Send(localizer.Tr(c.Sender().LanguageCode, "admin-update"))
	})

	admin.Handle("/change", func(c telebot.Context) error {
		msg, action, err := i.Listen(cauliflower.Parameters{
			Context: c,
			Message: localizer.Tr(c.Sender().LanguageCode, "admin-change-ask-action"),
		})
		if err != nil {
			return nil
		}

		msg, value, err := i.Listen(cauliflower.Parameters{
			Context: c,
			Message: localizer.Tr(c.Sender().LanguageCode, "admin-change-ask-value"),
			Edit:    msg,
		})
		if err != nil {
			return nil
		}

		switch action.Text {
		case "nofap-channel":
			config.NofapChannel = value.Text

		case "personal-channel":
			config.PersonalChannel = value.Text

		case "add-owner":
			owner, err := strconv.ParseInt(value.Text, 10, 64)

			if err != nil {
				c.Send(err)
				return err
			}

			config.Owners = append(config.Owners, owner)

		case "remove-owner":
			owner, err := strconv.ParseInt(value.Text, 10, 64)

			if err != nil {
				c.Send(err)
				return err
			}

			removeSlice(config.Owners, owner)

		default:
			_, err = b.Edit(msg, localizer.Tr(c.Sender().LanguageCode, "admin-change-failed", action.Text, value.Text))
			return err
		}

		_, err = b.Edit(msg, localizer.Tr(c.Sender().LanguageCode, "admin-change-success", action.Text, value.Text))
		return err
	})

	admin.Handle("/add-task", func(c telebot.Context) error {
		return nil
	})

	admin.Handle("/send", func(c telebot.Context) error {
		msg, answer, err := i.Listen(cauliflower.Parameters{
			Context: c,
			Message: "Enter the message to send",
		})
		if err != nil {
			return err
		}

		var users []User
		db.Find(&users)

		for _, u := range users {
			_, err = b.Send(u, answer.Text)
			if err != nil {
				c.Send("error with id: " + strconv.FormatInt(u.ID, 10))
			}
		}

		_, err = b.Edit(msg, "done")
		return err
	})

	admin.Handle("/dummy", func(c telebot.Context) error {
		db.Create(&User{
			ID:       c.Sender().ID,
			Username: c.Sender().Username,
		})

		db.Create(&Journey{
			CreatedAtStr: time.Now().Format("02 Jan 06 15:04"),
			UserID:       c.Sender().ID,
			RankSystem:   "memes",
			Start:        time.Now(),
		})

		db.Create(&Entry{
			CreatedAtStr: time.Now().Format("02 Jan 06 15:04"),
			UserID:       c.Sender().ID,
			IsPublic:     true,
			Note:         7,
			Text:         "lzihfhlfih",
		})

		db.Create(&Task{
			CreatedAtStr: time.Now().Format("02 Jan 06 15:04"),
			UserID:       c.Sender().ID,
			TaskID:       1,
			Text:         "abc",
			IsDone:       false,
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

	b.Handle(&button, func(c telebot.Context) error {
		return profileEntries(c, "public", func(c telebot.Context) error {
			id, err := strconv.ParseInt(c.Callback().Data, 10, 64)
			if err != nil {
				return err
			}

			var user User
			db.FirstOrCreate(&user, User{ID: id})
			return profile(c, user)
		})
	})

	markup.Inline(markup.Row(button))

	return c.EditOrSend(text, markup)
}

func profileEntries(c telebot.Context, privacy string, backHandler func(c telebot.Context) error) error {
	data := strings.Split(c.Callback().Data, "|")

	var user User
	if r := db.First(&user, data[0]); errors.Is(r.Error, gorm.ErrRecordNotFound) {
		return c.Send(localizer.Tr(c.Sender().LanguageCode, "entries-no-account"))
	}

	page, err := strconv.Atoi(data[1])
	if err != nil {
		return c.Send(localizer.Tr(c.Sender().LanguageCode, "err-button"))
	}

	var count int64
	var entries []Entry
	var textPrivacy string

	switch privacy {
	case "all":
		db.Model(&Entry{UserID: user.ID}).Count(&count)
		db.Limit(10).Offset((page-1)*10).Find(&entries, Entry{UserID: user.ID})
		textPrivacy = localizer.Tr(c.Sender().LanguageCode, "profile-entries-all")
	case "public":
		db.Model(&Entry{UserID: user.ID, IsPublic: true}).Count(&count)
		db.Limit(10).Offset((page-1)*10).Find(&entries, Entry{UserID: user.ID, IsPublic: true})
		textPrivacy = localizer.Tr(c.Sender().LanguageCode, "profile-entries-public")
	case "private":
		db.Model(&Entry{UserID: user.ID, IsPublic: false}).Count(&count)
		db.Limit(10).Offset((page-1)*10).Find(&entries, Entry{UserID: user.ID, IsPublic: false})
		textPrivacy = localizer.Tr(c.Sender().LanguageCode, "profile-entries-private")
	default:
		return errors.New("error with profileEntries privacy")
	}

	text := localizer.Tr(c.Sender().LanguageCode, "profile-entries", map[string]interface{}{
		"User":    user.Username,
		"Page":    page,
		"MaxPage": count/10 + 1,
		"Privacy": textPrivacy,
		"Entries": entries,
	})

	markup := b.NewMarkup()

	var previous, next telebot.Btn

	if page > 1 {
		previous = markup.Data(localizer.Tr(c.Sender().LanguageCode, "pagination-previous"), randomString(16), strconv.FormatInt(user.ID, 10), strconv.Itoa(page-1))
	}

	if int(count/10) > page {
		next = markup.Data(localizer.Tr(c.Sender().LanguageCode, "pagination-next"), randomString(16), strconv.FormatInt(user.ID, 10), strconv.Itoa(page+1))
	}

	back := markup.Data(localizer.Tr(c.Sender().LanguageCode, "pagination-back"), randomString(16), strconv.FormatInt(user.ID, 10))

	markup.Inline(markup.Row(previous, next, back))

	b.Handle(&next, func(c telebot.Context) error {
		return profileEntries(c, "public", func(c telebot.Context) error {
			id, err := strconv.ParseInt(c.Callback().Data, 10, 64)
			if err != nil {
				return err
			}

			var user User
			db.FirstOrCreate(&user, User{ID: id})
			return profile(c, user)
		})
	})

	b.Handle(&previous, func(c telebot.Context) error {
		return profileEntries(c, "public", func(c telebot.Context) error {
			id, err := strconv.ParseInt(c.Callback().Data, 10, 64)
			if err != nil {
				return err
			}

			var user User
			db.FirstOrCreate(&user, User{ID: id})
			return profile(c, user)
		})
	})

	b.Handle(&back, backHandler)

	return c.Edit(text, markup)
}

func commandStart(c telebot.Context) error {
	db.Save(&User{
		ID:       c.Sender().ID,
		Username: c.Sender().Username,
	})

	return c.Send(localizer.Tr(c.Sender().LanguageCode, "start-hello"))
}

func commandNew(c telebot.Context) error {
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
		CreatedAtStr: time.Now().Format("02 Jan 06 15:04"),
		UserID:       c.Sender().ID,
		Start:        start,
	})

	markup := b.NewMarkup()

	var buttons []telebot.Btn

	for _, rank := range config.Ranks {
		button := markup.Data(rank.Name, randomString(16), rank.Name)
		buttons = append(buttons, button)

		b.Handle(&button, markupNew)
	}

	markup.Inline(markup.Split(2, buttons)...)

	_, err = b.Edit(msg, text, markup)
	return err
}

func commandCheck(c telebot.Context) error {
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

	b.Handle(&relapsed, markupCheckRelapsed)
	b.Handle(&survived, markupCheckSurvived)

	markup.Inline(markup.Row(relapsed, survived))

	return c.Send(localizer.Tr(c.Sender().LanguageCode, "check-ask-relapsed"), markup)
}

func commandTask(c telebot.Context) error {
	now, midnight := today()

	var count int64
	db.Model(&Task{}).Where("user_id = ? AND updated_at BETWEEN ? AND ?", c.Sender().ID, now, midnight).Count(&count)
	if int(count) >= 3 {
		return c.Send(localizer.Tr(c.Sender().LanguageCode, "task-too-much"))
	}

	var task Task

	if r := db.First(&task, "user_id = ? AND is_done = ?", c.Sender().ID, false); r.RowsAffected > 0 {
		chat, err := b.ChatByID(task.UserID)
		if err != nil {
			return err
		}

		msg := telebot.Message{
			ID:   task.MessageID,
			Chat: chat,
		}

		_, err = b.Reply(&msg, localizer.Tr(c.Sender().LanguageCode, "task-unfinished"))
		return err
	}

	taskID := rand.Intn(len(config.Tasks))
	taskText := localizer.Tr(c.Sender().LanguageCode, config.Tasks[taskID].Task)

	text := localizer.Tr(c.Sender().LanguageCode, "task-cta", taskText, time.Now().Format("02 Jan 06 15:04"))

	markup := b.NewMarkup()

	button := markup.Data(localizer.Tr(c.Sender().LanguageCode, "task-button"), randomString(16))

	b.Handle(&button, markupTaskDone)

	markup.Inline(markup.Row(button))

	msg, err := b.Send(c.Chat(), text, markup)
	if err != nil {
		return err
	}

	db.Create(&Task{
		CreatedAtStr: time.Now().Format("02 Jan 06 15:04"),
		UserID:       c.Sender().ID,
		MessageID:    msg.ID,
		TaskID:       taskID,
		Text:         taskText,
		IsDone:       false,
	})

	return nil
}

func commandMotivation(c telebot.Context) error {
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
}

func commandProfile(c telebot.Context) error {
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
}

func commandAccount(c telebot.Context) error {
	var text string

	var j Journey
	if r := db.First(&j, "user_id = ?", c.Sender().ID); errors.Is(r.Error, gorm.ErrRecordNotFound) {
		// user doesn't have journeys
		return c.Send(localizer.Tr(c.Sender().LanguageCode, "account-text-no-journey"))
	}

	var a []Journey
	db.Select("start").Find(&a, &Journey{
		UserID: c.Sender().ID,
	})

	var totalDays int
	for _, j := range a {
		totalDays += int(time.Now().Sub(j.Start).Hours() / 24)
	}

	averageDays := totalDays

	if totalDays > 0 && len(a) > 0 {
		averageDays = totalDays / len(a)
	}

	var entriesCount, tasksCount int64
	db.Model(&Entry{}).Where("user_id = ?", c.Sender().ID).Count(&entriesCount)
	db.Model(&Task{}).Where("user_id = ?", c.Sender().ID).Count(&tasksCount)

	_, currentRank := getRank(j.Start, j.RankSystem, 0)
	daysLeft, nextRank := getRank(j.Start, j.RankSystem, 1)

	text = localizer.Tr(
		c.Sender().LanguageCode,
		"account-text",
		calculateScore(c.Sender().ID, true),
		currentRank,
		nextRank,
		daysLeft,
		totalDays,
		averageDays,
		entriesCount,
		tasksCount,
	)

	markup := b.NewMarkup()

	activity := markup.Data(localizer.Tr(c.Sender().LanguageCode, "account-activity"), randomString(16))
	entries := markup.Data(localizer.Tr(c.Sender().LanguageCode, "account-entries"), randomString(16), strconv.FormatInt(c.Sender().ID, 10), "1")
	download := markup.Data(localizer.Tr(c.Sender().LanguageCode, "account-download"), randomString(16))

	b.Handle(&activity, markupAccountActivity)
	b.Handle(&download, markupAccountDownload)
	b.Handle(&entries, func(c telebot.Context) error {
		return profileEntries(c, "all", commandAccount)
	})

	markup.Inline(
		markup.Row(activity, entries),
		markup.Row(download),
	)

	return c.EditOrSend(text, markup)
}

func commandRanks(c telebot.Context) error {
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
}

func commandHelp(c telebot.Context) error {
	var users int64
	db.Model(&Journey{}).Distinct("user_id").Count(&users)

	var totalResponseTime time.Duration
	for _, t := range responseTime {
		totalResponseTime += t
	}

	averageResponseTime := totalResponseTime / time.Duration(len(responseTime))

	return c.Send(localizer.Tr(
		c.Sender().LanguageCode,
		"help-text",
		users,
		messageCount,
		averageResponseTime,
		start.Format("02 Jan 06 15:04"),
		config.NofapChannel,
		config.PersonalChannel,
	))
}

func commandFix(c telebot.Context) error {
	db.Save(&User{
		ID:       c.Sender().ID,
		Username: c.Sender().Username,
	})

	return c.Send(localizer.Tr(c.Sender().LanguageCode, "fix-text"))
}

func markupNew(c telebot.Context) error {
	var j Journey

	db.Model(&j).Where("user_id = ? AND end = ?", c.Sender().ID, time.Time{}).Updates(Journey{RankSystem: c.Callback().Data}).First(&j)

	_, rank := getRank(j.Start, j.RankSystem, 0)

	text := localizer.Tr(
		c.Sender().LanguageCode,
		"new-saved",
		rank,
		j.RankSystem,
		j.Start.Format("02 Jan 06"),
		int(time.Now().Sub(j.Start).Hours()/24),
	)

	// markup := &telebot.ReplyMarkup{ResizeKeyboard: true}
 
	// motivation := markup.Text(localizer.Tr(c.Sender().LanguageCode, "markup-motivation"))
	// account := markup.Text(localizer.Tr(c.Sender().LanguageCode, "markup-account"))
	// check := markup.Text(localizer.Tr(c.Sender().LanguageCode, "markup-check"))
	// task := markup.Text(localizer.Tr(c.Sender().LanguageCode, "markup-task"))
 
	// b.Handle(&motivation, commandMotivation)
	// b.Handle(&account, commandAccount)
	// b.Handle(&check, commandCheck)
	// b.Handle(&task, commandTask)
 
	// markup.Reply(
	// 	markup.Row(motivation, account),
	// 	markup.Row(check, task),
	// )

	return c.Edit(text)
}

func markupCheckRelapsed(c telebot.Context) error {
	msg, answer, err := i.Listen(cauliflower.Parameters{
		Context: c,
		Message: localizer.Tr(c.Sender().LanguageCode, "relapsed"),
		Edit:    c.Message(),
	})
	if err != nil {
		return nil
	}

	db.Where("user_id = ?", c.Sender().ID).Updates(&Journey{
		End:  time.Now(),
		Text: answer.Text,
	})

	// markup := &telebot.ReplyMarkup{ResizeKeyboard: true}
 
	// motivation := markup.Text(localizer.Tr(c.Sender().LanguageCode, "markup-motivation"))
	// account := markup.Text(localizer.Tr(c.Sender().LanguageCode, "markup-account"))
	// new := markup.Text(localizer.Tr(c.Sender().LanguageCode, "markup-new"))
 
	// b.Handle(&motivation, commandMotivation)
	// b.Handle(&account, commandAccount)
	// b.Handle(&new, commandNew)
 
	// markup.Reply(
	// 	markup.Row(motivation, account),
	// 	markup.Row(new),
	// )

	_, err = b.Edit(msg, localizer.Tr(c.Sender().LanguageCode, "relapsed-saved"))
	return err
}

func markupCheckSurvived(c telebot.Context) error {
	markup := b.NewMarkup()

	var buttons []telebot.Btn

	for n := 1; n < 11; n++ {
		text := strconv.Itoa(n)
		button := markup.Data(text, randomString(16), text)

		b.Handle(&button, markupCheckSurvivedNote)

		buttons = append(buttons, button)
	}

	markup.Inline(markup.Split(5, buttons)...)

	return c.Edit(localizer.Tr(c.Sender().LanguageCode, "survived-ask-note"), markup)
}

func markupCheckSurvivedNote(c telebot.Context) error {
	data := strings.TrimSpace(c.Callback().Data)

	number, err := strconv.Atoi(data)
	if err != nil {
		return err
	}

	msg, answer, err := i.Listen(cauliflower.Parameters{
		Context: c,
		Message: localizer.Tr(c.Sender().LanguageCode, "survived-ask-entry"),
		Edit:    c.Message(),
	})
	if err != nil {
		return nil
	}

	db.Create(&Entry{
		CreatedAtStr: time.Now().Format("02 Jan 06 15:04"),
		UserID:       c.Sender().ID,
		IsPublic:     false,
		Note:         number,
		Text:         answer.Text,
	})

	markup := b.NewMarkup()

	public := markup.Data(localizer.Tr(c.Sender().LanguageCode, "survived-button-public"), "public")
	private := markup.Data(localizer.Tr(c.Sender().LanguageCode, "survived-button-private"), "private")

	b.Handle(&public, func(c telebot.Context) error {
		return handlePrivacy(c, true, number, answer.Text)
	})

	b.Handle(&private, func(c telebot.Context) error {
		return handlePrivacy(c, false, number, answer.Text)
	})

	markup.Inline(markup.Row(public, private))

	_, err = b.Edit(msg, localizer.Tr(c.Sender().LanguageCode, "survived-ask-public"), markup)
	return err
}

func markupTaskDone(c telebot.Context) error {
	var task Task

	if r := db.First(&task, "user_id = ? AND is_done = ?", c.Sender().ID, false); r.RowsAffected > 0 {
		if r := db.Model(&task).Updates(Task{IsDone: true}); r.Error != nil {
			log.Println(r.Error)
		}

		taskText := localizer.Tr(c.Sender().LanguageCode, config.Tasks[task.TaskID].Task)
		taskPoints := config.Tasks[task.TaskID].Points

		text := localizer.Tr(c.Sender().LanguageCode, "task-done", taskText, task.CreatedAt.Format("02 Jan 06 15:04"), task.UpdatedAt.Format("02 Jan 06 15:04"), taskPoints)

		return c.Edit(text)
	}

	return nil
}

func markupAccountActivity(c telebot.Context) error {
	var journeys []Journey
	var entries []Entry
	var tasks []Task

	db.Find(&journeys, "user_id = ?", c.Sender().ID)
	db.Find(&entries, "user_id = ?", c.Sender().ID)
	db.Find(&tasks, "user_id = ?", c.Sender().ID)

	var activities []Activity

	for _, j := range journeys {
		activities = append(activities, Activity{CreatedAt: j.CreatedAt, Type: "journey", Item: j})
	}

	for _, e := range entries {
		activities = append(activities, Activity{CreatedAt: e.CreatedAt, Type: "entry", Item: e})
	}

	for _, t := range tasks {
		activities = append(activities, Activity{CreatedAt: t.CreatedAt, Type: "task", Item: t})
	}

	sort.SliceStable(activities, func(i, j int) bool {
		return activities[i].CreatedAt.Before(activities[j].CreatedAt)
	})

	markup := b.NewMarkup()

	back := markup.Data(localizer.Tr(c.Sender().LanguageCode, "pagination-back"), randomString(16))

	b.Handle(&back, commandAccount)

	markup.Inline(markup.Row(back))

	return c.Edit(localizer.Tr(c.Sender().LanguageCode, "account-activity-text", activities), markup)
}

func markupAccountDownload(c telebot.Context) error {
	c.Notify(telebot.UploadingDocument)

	var activities []Activity
	var journeys []Journey
	var entries []Entry
	var tasks []Task

	db.Find(&journeys, "user_id = ?", c.Sender().ID)
	db.Find(&entries, "user_id = ?", c.Sender().ID)
	db.Find(&tasks, "user_id = ?", c.Sender().ID)

	for _, j := range journeys {
		activities = append(activities, Activity{CreatedAt: j.CreatedAt, Type: "journey", Item: j})
	}

	for _, e := range entries {
		activities = append(activities, Activity{CreatedAt: e.CreatedAt, Type: "entry", Item: e})
	}

	for _, t := range tasks {
		activities = append(activities, Activity{CreatedAt: t.CreatedAt, Type: "task", Item: t})
	}

	sort.SliceStable(activities, func(i, j int) bool {
		return activities[i].CreatedAt.Before(activities[j].CreatedAt)
	})

	data := map[string]interface{}{
		"activity": activities,
		"journeys": journeys,
		"entries":  entries,
		"tasks":    tasks,
	}

	marshaled, err := yaml.Marshal(&data)
	if err != nil {
		return err
	}

	document := telebot.Document{
		File:     telebot.FromReader(bytes.NewReader(marshaled)),
		Caption:  localizer.Tr(c.Sender().LanguageCode, "account-download-document"),
		MIME:     "text/yaml",
		FileName: "data.yml",
	}

	_, err = document.Send(b, c.Sender(), &telebot.SendOptions{})

	c.Respond()

	return err
}

func update() error {
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

	cm = closestmatch.New(removeDuplicate(matches), []int{2})

	return nil
}

func handlePrivacy(c telebot.Context, isPublic bool, number int, answer string) error {
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
		Text:   answer,
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
	var entries int64

	if allJourneys {
		var journeys []Journey
		db.Select("start", "end").Where("user_id = ?", userID).Find(&journeys)

		for _, j := range journeys {
			end := j.End
			if end.IsZero() {
				end = time.Now()
			}
			score += int(end.Sub(j.Start).Hours()/24) * 2
		}

		db.Select("task_id").Where("user_id = ?", userID).Find(&tasks)
		db.Model(&Entry{}).Where("user_id = ?", userID).Count(&entries)
	} else {
		var j Journey
		db.Select("start").Where("user_id = ? AND end = ?", userID, time.Time{}).Last(&j)

		if !j.Start.IsZero() {
			score += int(time.Now().Sub(j.Start).Hours()/24) * 2
			db.Select("task_id").Where("user_id = ? AND updated_at > ?", userID, j.Start).Find(&tasks)
			db.Model(&Entry{}).Where("user_id = ? AND created_at > ?", userID, j.Start).Count(&entries)
		}
	}

	for _, task := range tasks {
		score += config.Tasks[task.TaskID].Points
	}

	score += int(entries)

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

func removeSlice[T comparable](l []T, item T) []T {
	for i, other := range l {
		if other == item {
			return append(l[:i], l[i+1:]...)
		}
	}
	return l
}
