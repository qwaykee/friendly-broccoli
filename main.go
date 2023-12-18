package main

import (
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/qwaykee/cauliflower"
	"github.com/schollz/closestmatch"
	"gopkg.in/telebot.v3"
	"gopkg.in/telebot.v3/middleware"
	"gopkg.in/telebot.v3/layout"
	"gopkg.in/yaml.v3"

	"bytes"
	_ "embed"
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
	db        *gorm.DB
	lt        *layout.Layout
	b         *telebot.Bot
	i         *cauliflower.Instance
	cm        *closestmatch.ClosestMatch

	messageCount          int
	responseTime          []time.Duration
	motivationsCategories = make(map[string]int)
	notesMarkup           *telebot.ReplyMarkup
	ranksMarkup           *telebot.ReplyMarkup
	start                 time.Time
	usersLanguage 		  = make(map[int64]string)

	// config
	owners []int64
	ranks = make(map[string]Rank)
)

func init() {
	var err error
	log.Println("initialization")

	// load layout
	lt, err = layout.New("bot.yml")
	if err != nil {
		log.Fatalf("layout: %v", err)
	}

	if err := lt.UnmarshalKey("owners", &owners); err != nil {
		log.Fatalf("layout owners: %v", err)
	}

	if err := lt.UnmarshalKey("ranks", &ranks); err != nil {
		log.Fatalf("layout ranks: %v", err)
	}

	// initialize database
	db, err = gorm.Open(sqlite.Open(lt.String("database")), &gorm.Config{PrepareStmt: true})
	if err != nil {
		log.Fatalf("gorm: %v", err)
	}

	db.AutoMigrate(&User{}, &Journey{}, &Entry{}, &Task{}, &Motivation{}, &TaskData{})

	// load motivation images into db and closest matches
	if err := update(); err != nil {
		log.Fatalf("updater motivation: %v", err)
	}

	// create bot and set commands
	b, err = telebot.NewBot(lt.Settings())
	if err != nil {
		log.Fatalf("telebot: %v", err)
	}

	if lt.Bool("set_commands") {
		if err := b.SetCommands(lt.Commands()); err != nil {
			log.Fatalf("telebot: %v", err)
		}
	}

	// initialize cauliflower
	i, err = cauliflower.NewInstance(&cauliflower.Settings{
		Bot:    b,
		InstallMiddleware: true,
		DefaultListen: &cauliflower.ListenOptions{
			Cancel: "/cancel",
			TimeoutHandler: func(c telebot.Context) error {
				return c.Send(lt.Text(c, "err-no-message-received"))
			},
			CancelHandler: func(c telebot.Context) error {
				return c.Send(lt.Text(c, "err-command-canceled"))
			},
		},
	})
	if err != nil {
		log.Fatalf("cauliflower: %v", err)
	}

	notesMarkup, err = i.Keyboard(&cauliflower.KeyboardOptions{
		Keyboard: cauliflower.Inline,
		Row: []string{"1", "2", "3", "4", "5", "6", "7", "8", "9", "10"},
		Split: 5,
		Handler: markupCheckSurvivedNote,
	})
	if err != nil {
		log.Fatalf("keyboard: %v", err)
	}

	data := make([]string, len(ranks))
	for _, rank := range ranks {data = append(data, rank.Name)}

	ranksMarkup, err = i.Keyboard(&cauliflower.KeyboardOptions{
		Keyboard: cauliflower.Inline,
		Row: data,
		Split: 2,
		Handler: markupNew,
	})
	if err != nil {
		log.Fatalf("keyboard: %v", err)
	}

	start = time.Now()
}

func main() {
	// handle message count, response time and language
	b.Use(func(next telebot.HandlerFunc) telebot.HandlerFunc {
		return func(c telebot.Context) error {
			start := time.Now()

			messageCount += 1

			if _, ok := usersLanguage[c.Chat().ID]; !ok {
				usersLanguage[c.Chat().ID] = c.Sender().LanguageCode
			}

			err := next(c)

			responseTime = append(responseTime, time.Now().Sub(start))

			return err
		}
	})

	b.Use(lt.Middleware("fr", func(r telebot.Recipient) string {
		userID, err := strconv.ParseInt(r.Recipient(), 10, 64)
		if err != nil {
			log.Printf("i18n middleware strconv: %v", err)
		}

		if lang, ok := usersLanguage[userID]; ok {
			return lang
		}

		return "fr"
	}))

	b.Use(middleware.AutoRespond())

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

	admin.Use(middleware.Whitelist(owners...))

	admin.Handle("/update", func(c telebot.Context) error {
		if err := update(); err != nil {
			return err
		}

		return c.Send(lt.Text(c, "admin-update"))
	})

	admin.Handle("/change", func(c telebot.Context) error {
		msg, action, err := i.Listen(&cauliflower.ListenOptions{
			Context: c,
			Message: lt.Text(c, "admin-change-ask-action"),
		})
		if err != nil {
			return nil
		}

		msg, value, err := i.Listen(&cauliflower.ListenOptions{
			Context: c,
			Message: lt.Text(c, "admin-change-ask-value"),
			Edit:    msg,
		})
		if err != nil {
			return nil
		}

		switch action.Text {
		//case "nofap-channel":
		//	lt.Config.v.Set("channels.bot", value.Text)
//
		//case "personal-channel":
		//	lt.Config.v.Set("channels.personal", value.Text)

		case "add-owner":
			owner, err := strconv.ParseInt(value.Text, 10, 64)

			if err != nil {
				c.Send(err)
				return err
			}

			owners = append(owners, owner)

		case "remove-owner":
			owner, err := strconv.ParseInt(value.Text, 10, 64)

			if err != nil {
				c.Send(err)
				return err
			}

			removeSlice(owners, owner)

		default:
			_, err = b.Edit(msg, lt.Text(c, "admin-change-failed", map[string]any{
				"Action": action.Text,
				"Value": value.Text,
			}))
			return err
		}

		_, err = b.Edit(msg, lt.Text(c, "admin-change-success", map[string]any{
			"Action": action.Text,
			"Value": value.Text,
		}))
		return err
	})

	admin.Handle("/add-task", func(c telebot.Context) error {
		points, err := strconv.Atoi(c.Args()[0])
		if err != nil {
			c.Send(lt.Text(c, "admin-error-convert-atoi", c.Args()[0]))
		}

		db.Create(&TaskData{
			Points: points,
			Task: c.Args()[1],
		})

		return c.Send(lt.Text(c, "admin-update"))
	})

	admin.Handle("/send", func(c telebot.Context) error {
		msg, answer, err := i.Listen(&cauliflower.ListenOptions{
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
		return c.Send(lt.Text(c, "profile-text-no-journey"))
	}

	var translationKey string
	if j.End.IsZero() {
		translationKey = "profile-current-journey"
	} else {
		translationKey = "profile-last-journey"
	}

	journeyIsCurrent := lt.Text(c, translationKey)

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

	text := lt.Text(c, "profile-text", map[string]any{
		"Username": user.Username, // current/last journey
		"TotalScore": totalScore,
		"JourneyIsCurrent": journeyIsCurrent,
		"CurrentScore": currentScore,
		"Start": j.Start.Format("02 Jan 06"),
		"Days": days,
		"CurrentRank": currentRank,
		"NextRank": nextRank,
		"DaysLeft": daysLeft,
		"EntriesCount": entriesCount,
		"TasksCount": tasksCount,
		"JourneysCount": len(a), // all journeys
		"AverageDays": averageDays,
		"TotalDays": totalDays,
		"TotalEntriesCount": totalEntriesCount,
		"TotalTasksCount": totalTasksCount,
	})

	markup := b.NewMarkup()

	button := markup.Data(lt.Text(c, "profile-button", user), randomString(16), strconv.FormatInt(user.ID, 10), "1")

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
		return c.Send(lt.Text(c, "entries-no-account"))
	}

	page, err := strconv.Atoi(data[1])
	if err != nil {
		return c.Send(lt.Text(c, "err-button"))
	}

	var count int64
	var entries []Entry
	var textPrivacy string

	switch privacy {
	case "all":
		db.Model(&Entry{UserID: user.ID}).Count(&count)
		db.Limit(10).Offset((page-1)*10).Find(&entries, Entry{UserID: user.ID})
		textPrivacy = lt.Text(c, "profile-entries-all")
	case "public":
		db.Model(&Entry{UserID: user.ID, IsPublic: true}).Count(&count)
		db.Limit(10).Offset((page-1)*10).Find(&entries, Entry{UserID: user.ID, IsPublic: true})
		textPrivacy = lt.Text(c, "profile-entries-public")
	case "private":
		db.Model(&Entry{UserID: user.ID, IsPublic: false}).Count(&count)
		db.Limit(10).Offset((page-1)*10).Find(&entries, Entry{UserID: user.ID, IsPublic: false})
		textPrivacy = lt.Text(c, "profile-entries-private")
	default:
		return errors.New("error with profileEntries privacy")
	}

	text := lt.Text(c, "profile-entries", map[string]interface{}{
		"User":    user.Username,
		"Page":    page,
		"MaxPage": count/10 + 1,
		"Privacy": textPrivacy,
		"Entries": entries,
	})

	markup := b.NewMarkup()

	var previous, next telebot.Btn

	if page > 1 {
		previous = markup.Data(lt.Text(c, "pagination-previous"), randomString(16), strconv.FormatInt(user.ID, 10), strconv.Itoa(page-1))
	}

	if int(count/10) > page {
		next = markup.Data(lt.Text(c, "pagination-next"), randomString(16), strconv.FormatInt(user.ID, 10), strconv.Itoa(page+1))
	}

	back := markup.Data(lt.Text(c, "pagination-back"), randomString(16), strconv.FormatInt(user.ID, 10))

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

	return c.Send(lt.Text(c, "start-hello"))
}

func commandNew(c telebot.Context) error {
	var found bool
	db.Raw("SELECT EXISTS(SELECT 1 FROM journeys WHERE user_id = ? AND end = ?) AS found", c.Sender().ID, time.Time{}).Scan(&found)
	if found {
		return c.Send(lt.Text(c, "new-already-running-journey"))
	}

	msg, answer, err := i.Listen(&cauliflower.ListenOptions{
		Context: c,
		Message: lt.Text(c, "new-ask-streak"),
	})
	if err != nil {
		return nil
	}

	days, err := strconv.Atoi(answer.Text)
	if err != nil {
		return c.Send(lt.Text(c, "new-not-a-number"))
	}

	start := time.Now().Add(-time.Duration(days) * time.Hour * 24)
	text := lt.Text(c, "new-ask-rank", map[string]any{
		"Start": start.Format("02 Jan 06"),
	})

	db.Create(&Journey{
		CreatedAtStr: time.Now().Format("02 Jan 06 15:04"),
		UserID:       c.Sender().ID,
		Start:        start,
	})

	_, err = b.Edit(msg, text, ranksMarkup)
	return err
}

func commandCheck(c telebot.Context) error {
	var found bool
	db.Raw("SELECT EXISTS(SELECT 1 FROM journeys WHERE user_id = ? AND end = ?) AS found", c.Sender().ID, time.Time{}).Scan(&found)
	if !found {
		return c.Send(lt.Text(c, "check-no-journey"))
	}

	now, midnight := today()

	var count int64
	db.Model(&Entry{}).Where("created_at BETWEEN ? AND ?", midnight, now).Count(&count)
	if int(count) >= 3 {
		return c.Send(lt.Text(c, "check-already-checked-in"))
	}

	markup := b.NewMarkup()

	relapsed := markup.Data(lt.Text(c, "check-button-relapsed"), "relapsed")
	survived := markup.Data(lt.Text(c, "check-button-survived"), "survived")

	b.Handle(&relapsed, markupCheckRelapsed)
	b.Handle(&survived, markupCheckSurvived)

	markup.Inline(markup.Row(relapsed, survived))

	return c.Send(lt.Text(c, "check-ask-relapsed"), markup)
}

func commandTask(c telebot.Context) error {
	now, midnight := today()

	var count int64
	db.Model(&Task{}).Where("user_id = ? AND updated_at BETWEEN ? AND ?", c.Sender().ID, now, midnight).Count(&count)
	if int(count) >= 3 {
		return c.Send(lt.Text(c, "task-too-much"))
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

		_, err = b.Reply(&msg, lt.Text(c, "task-unfinished"))
		return err
	}

	var taskData TaskData
	db.Order("RANDOM()").Take(&taskData)
	taskText := lt.Text(c, taskData.Task)

	text := lt.Text(c, "task-cta",map[string]any{
		"Task": taskText,
		"Now": time.Now().Format("02 Jan 06 15:04"),
	})

	markup := b.NewMarkup()

	button := markup.Data(lt.Text(c, "task-button"), randomString(16))

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
		TaskID:       int(taskData.ID),
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
			Caption: lt.Text(c, "motivation-caption", m),
		})
	}

	arg := c.Args()[0]

	if arg == "list" {
		return c.Send(lt.Text(c, "motivation-list", motivationsCategories))
	}

	c.Notify(telebot.UploadingPhoto)

	var m Motivation

	if r := db.Where("pack = ? OR id = ? OR category = ?", arg, arg, arg).Order("RANDOM()").Take(&m); r.RowsAffected == 0 {
		return c.Send(lt.Text(c, "motivation-error", cm.Closest(arg)))
	}

	if m.Pack != "" {
		return sendPack(c, m)
	}

	return c.Send(&telebot.Photo{
		File:    telebot.FromDisk(m.Path),
		Caption: lt.Text(c, "motivation-caption", m),
	})
}

func commandProfile(c telebot.Context) error {
	var user User

	if len(c.Args()) > 0 {
		username := strings.Trim(c.Args()[0], "@")

		if r := db.Last(&user, "username = ?", username); errors.Is(r.Error, gorm.ErrRecordNotFound) {
			// user doesn't exist
			return c.Send(lt.Text(c, "profile-text-no-journey"))
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
		return c.Send(lt.Text(c, "account-text-no-journey"))
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

	text = lt.Text(c, "account-text", map[string]any{
		"Score": calculateScore(c.Sender().ID, true),
		"CurrentRank": currentRank,
		"NextRank": nextRank,
		"DaysLeft": daysLeft,
		"TotalDays": totalDays,
		"AverageDays": averageDays,
		"EntriesCount": entriesCount,
		"TasksCount": tasksCount,
	})

	markup := b.NewMarkup()

	activity := markup.Data(lt.Text(c, "account-activity"), randomString(16))
	entries := markup.Data(lt.Text(c, "account-entries"), randomString(16), strconv.FormatInt(c.Sender().ID, 10), "1")
	download := markup.Data(lt.Text(c, "account-download"), randomString(16))

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
		rank := ranks[c.Args()[0]]

		text += "*" + rank.Name + "*\n"

		sortedLevels := maps.Keys(rank.Levels)
		sort.Ints(sortedLevels)

		for _, level := range sortedLevels {
			text += strconv.Itoa(level) + ": " + rank.Levels[level] + "\n"
		}
	} else {
		for _, rank := range ranks {
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

	return c.Send(lt.Text(c, "help-text", map[string]any{
		"UsersCount": users,
		"MessageCount": messageCount,
		"AverageResponseTime": averageResponseTime,
		"Uptime": start.Format("02 Jan 06 15:04"),
		"NofapChannel": lt.String("channels.bot"),
		"PersonalChannel": lt.String("channels.personal"),
	}))
}

func commandFix(c telebot.Context) error {
	db.Save(&User{
		ID:       c.Sender().ID,
		Username: c.Sender().Username,
	})

	return c.Send(lt.Text(c, "fix-text"))
}

func markupNew(c telebot.Context) error {
	var j Journey

	db.Model(&j).Where("user_id = ? AND end = ?", c.Sender().ID, time.Time{}).Updates(Journey{RankSystem: c.Callback().Data}).First(&j)

	_, rank := getRank(j.Start, j.RankSystem, 0)

	return c.Edit(lt.Text(c, "new-saved", map[string]any{
		"Rank": rank,
		"RankSystem": j.RankSystem,
		"Start": j.Start.Format("02 Jan 06"),
		"Days": int(time.Now().Sub(j.Start).Hours()/24),
	}))
}

func markupCheckRelapsed(c telebot.Context) error {
	msg, answer, err := i.Listen(&cauliflower.ListenOptions{
		Context: c,
		Message: lt.Text(c, "relapsed"),
		Edit:    c.Message(),
	})
	if err != nil {
		return nil
	}

	db.Where("user_id = ?", c.Sender().ID).Updates(&Journey{
		End:  time.Now(),
		Text: answer.Text,
	})

	_, err = b.Edit(msg, lt.Text(c, "relapsed-saved"))
	return err
}

func markupCheckSurvived(c telebot.Context) error {
	return c.Edit(lt.Text(c, "survived-ask-note"), notesMarkup)
}

func markupCheckSurvivedNote(c telebot.Context) error {
	data := strings.TrimSpace(c.Callback().Data)

	number, err := strconv.Atoi(data)
	if err != nil {
		return err
	}

	msg, answer, err := i.Listen(&cauliflower.ListenOptions{
		Context: c,
		Message: lt.Text(c, "survived-ask-entry"),
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

	public := markup.Data(lt.Text(c, "survived-button-public"), "public")
	private := markup.Data(lt.Text(c, "survived-button-private"), "private")

	b.Handle(&public, func(c telebot.Context) error {
		return handlePrivacy(c, true, number, answer.Text)
	})

	b.Handle(&private, func(c telebot.Context) error {
		return handlePrivacy(c, false, number, answer.Text)
	})

	markup.Inline(markup.Row(public, private))

	_, err = b.Edit(msg, lt.Text(c, "survived-ask-public"), markup)
	return err
}

func markupTaskDone(c telebot.Context) error {
	var task Task

	if r := db.First(&task, "user_id = ? AND is_done = ?", c.Sender().ID, false); r.RowsAffected > 0 {
		if r := db.Model(&task).Updates(Task{IsDone: true}); r.Error != nil {
			log.Println(r.Error)
		}

		var taskData TaskData
		db.First(&taskData, task.TaskID)

		text := lt.Text(c, "task-done", map[string]any{
			"Task": task.Text,
			"GivenAt": task.CreatedAt.Format("02 Jan 06 15:04"),
			"DoneAt": task.UpdatedAt.Format("02 Jan 06 15:04"),
			"Points": taskData.Points,
		})

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

	back := markup.Data(lt.Text(c, "pagination-back"), randomString(16))

	b.Handle(&back, commandAccount)

	markup.Inline(markup.Row(back))

	return c.Edit(lt.Text(c, "account-activity-text", activities), markup)
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
		Caption:  lt.Text(c, "account-download-document"),
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

	if err := filepath.Walk("motivation", func(path string, file fs.FileInfo, err error) error {
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
		privacy = lt.Text(c, "survived-public")
		command = "/profile"
	} else {
		privacy = lt.Text(c, "survived-private")
		command = "/account"
	}

	entry := &Entry{
		UserID: c.Sender().ID,
		Note:   number,
		Text:   answer,
	}

	db.Model(&entry).Where(&entry).Updates(Entry{IsPublic: isPublic})

	return c.Edit(lt.Text(c, "survived-saved", map[string]any{
		"Privacy": privacy,
		"Note": entry.Note,
		"Command": command,
		"Text": entry.Text,
	}))
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

	return c.Send(lt.Text(c, "motivation-caption", m))
}

func getRank(start time.Time, rank string, offset int) (int, string) {
	days := int(time.Now().Sub(start).Hours() / 24)
	levels := ranks[strings.ToLower(rank)].Levels

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
		var taskData TaskData
		db.First(&taskData, task.TaskID)

		score += taskData.Points
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
