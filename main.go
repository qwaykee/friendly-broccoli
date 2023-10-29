package main

import (
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/kataras/i18n"
	"github.com/qwaykee/cauliflower"
	"gopkg.in/telebot.v3"
	"gopkg.in/yaml.v3"

	"golang.org/x/exp/maps"
	"sort"
	"embed"
	"log"
	"strconv"
	"time"
	"strings"
	"math/rand"
)

var (
	config    Config
	localizer *i18n.I18n
	db        *gorm.DB
	b         *telebot.Bot
	i         *cauliflower.Instance

	messageCount  int
	chatToChannel = make(map[int64](*chan string))
	rankButtons   = []telebot.Btn{}
	start         time.Time

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
	// i18n.SetDefaultLanguage("en-US")

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

	db.AutoMigrate(&Journey{}, &Entry{}, &Task{})

	// create bot and set commands
	b, err = telebot.NewBot(telebot.Settings{
		Token:  config.Token,
		Poller: &telebot.LongPoller{Timeout: time.Duration(config.Timeout) * time.Second},
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
		Bot:               b,
		Cancel:            "/cancel",
		TimeoutHandler:    func(c telebot.Context) error {
			return c.Send(localizer.Tr(c.Sender().LanguageCode, "err-no-message-received"))
		},
		CancelHandler: 	   func(c telebot.Context) error {
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
	var ranksMarkup telebot.ReplyMarkup
	var buttons []telebot.Btn

	for _, option := range config.Ranks {
		button := ranksMarkup.Data(option.Name, option.Name, option.Name)
	    buttons = append(buttons, button)

	    b.Handle(&button, func(c telebot.Context) error {
			journey := &Journey{
				UserID: c.Sender().ID,
				End: time.Time{},
			}

			db.Last(&journey)

			journey.RankSystem = c.Callback().Data

			db.Save(&journey)

			_, rank := getRank(journey.Start, journey.RankSystem, 0)

			text := localizer.Tr(
				c.Sender().LanguageCode,
				"new-callback-success",
				rank,
				int(time.Now().Sub(journey.Start).Hours() / 24),
				journey.Start.Format("02 Jan 06"),
				journey.RankSystem,
			)

			_, err := b.Edit(c.Callback().Message, text)
			return err
	    })
	}

	ranksMarkup.Inline(ranksMarkup.Split(2, buttons)...)

	// initialize check-in buttons
	var relapsed, survived telebot.Btn

	// handle message count
	b.Use(func(next telebot.HandlerFunc) telebot.HandlerFunc {
		return func(c telebot.Context) error {
			messageCount += 1
			return next(c)
		}
	})

	b.Handle("/start", func(c telebot.Context) error {
		return c.Send(localizer.Tr(c.Sender().LanguageCode, "start-hello"))
	})

	b.Handle("/new", func(c telebot.Context) error {
		if r := db.Find(&Journey{UserID: c.Sender().ID}, "end IS NULL"); r.RowsAffected > 0 {
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
		if r := db.Last(&Journey{
			UserID: c.Sender().ID,
			End: time.Time{},
		}); r.Error == gorm.ErrRecordNotFound {
			return c.Send(localizer.Tr(c.Sender().LanguageCode, "check-no-journey"))
		}

		now := time.Now()
		midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())

		if r := db.Find(&Entry{}).Where("created_at BETWEEN ? AND ?", midnight, now); r.RowsAffected >= 3 {
			return c.Send(localizer.Tr(c.Sender().LanguageCode, "check-already-checked-in"))
		}

		checkInButtons := b.NewMarkup()

		relapsed = checkInButtons.Data(localizer.Tr(c.Sender().LanguageCode, "check-button-relapsed"), "relapsed")
		survived = checkInButtons.Data(localizer.Tr(c.Sender().LanguageCode, "check-button-survived"), "survived")

		b.Handle(&relapsed, func(c telebot.Context) error {
			msg, err := b.Edit(c.Message(), localizer.Tr(c.Sender().LanguageCode, "relapsed"))
			if err != nil {
				log.Println(err)
			}

			_, answer, err := i.Listen(cauliflower.Parameters{Context: c,})
			if err != nil {
				return nil
			}

			db.Save(&Journey{
				UserID: c.Sender().ID,
				End: time.Now(),
				Text: answer.Text,
			})

			_, err = b.Edit(msg, localizer.Tr(c.Sender().LanguageCode, "relapsed-done"))
			return err
		})

		b.Handle(&survived, func(c telebot.Context) error {
			noteButtons := sliceMarkup(5, []string{"1","2","3","4","5","6","7","8","9","10"})

			_, err := b.Edit(c.Message(), localizer.Tr(c.Sender().LanguageCode, "survived-ask-note"), noteButtons)

			return err
		})

		checkInButtons.Inline(checkInButtons.Split(2, []telebot.Btn{relapsed, survived})...)

		return c.Send(localizer.Tr(c.Sender().LanguageCode, "check-ask-relapsed"), checkInButtons)
	})

	b.Handle("/task", func(c telebot.Context) error {
		var task Task

		if r := db.Where("user_id = ? AND is_done = 0", c.Sender().ID).Find(&task); r.RowsAffected > 0 {
			chat, err := b.ChatByID(task.ChatID)
			if err != nil {
				log.Println(err)
			}

			message, err := strconv.Atoi(task.MessageID)
			if err != nil {
				log.Println(err)
			}

			msg := telebot.Message{
				ID: message,
				Chat: chat,
			}

			_, err = b.Reply(&msg, localizer.Tr(c.Sender().LanguageCode, "task-unfinished"))
			return err
		} else {
			taskID := rand.Intn(len(config.Tasks))
			taskText := localizer.Tr(c.Sender().LanguageCode, config.Tasks[taskID].Task)

			text := localizer.Tr(c.Sender().LanguageCode, "task-cta", taskText)

			markup := b.NewMarkup()
			buttonText := localizer.Tr(c.Sender().LanguageCode, "task-button")
			button := markup.Data(buttonText, randomString(16))

			b.Handle(&button, func(c telebot.Context) error {
				var task Task

				if r := db.Where("user_id = ? AND is_done = 0", c.Sender().ID).Find(&task); r.RowsAffected > 0 {
					task.IsDone = true

					db.Save(&task)

					taskText := localizer.Tr(c.Sender().LanguageCode, config.Tasks[task.TaskID].Task)
					taskPoints := config.Tasks[task.TaskID].Points

					text := localizer.Tr(c.Sender().LanguageCode, "task-done", taskText, taskPoints)

					_, err := b.Edit(c.Message(), text)
					return err
				}

				return nil
			})

			markup.Inline(markup.Row(button))

			msg, err := b.Send(c.Chat(), text, markup)
			if err != nil {
				log.Println(err)
			}

			db.Create(&Task{
				UserID: c.Sender().ID,
				ChatID: c.Chat().ID,
				MessageID: strconv.Itoa(msg.ID),
				TaskID: taskID,
				IsDone: false,
			})
		}
		return nil
	})

	b.Handle("/motivation" func(c telebot.Context) error {
		
	})

	b.Handle(telebot.OnCallback, func(c telebot.Context) error {
		data := strings.TrimSpace(c.Callback().Data)

		if number, err := strconv.Atoi(data); err == nil {
			// handle note from check
			msg, _ := b.Edit(c.Message(), localizer.Tr(c.Sender().LanguageCode, "survived-ask-entry"))

			_, answer, err := i.Listen(cauliflower.Parameters{
				Context: c,
			})
			if err != nil {
				return nil
			}

			db.Create(&Entry{
				UserID: c.Sender().ID,
				Note: number,
				Text: answer.Text,
			})

			privacyButtons := b.NewMarkup()

			public := privacyButtons.Data(localizer.Tr(c.Sender().LanguageCode, "survived-button-public"), "public")
			private := privacyButtons.Data(localizer.Tr(c.Sender().LanguageCode, "survived-button-private"), "private")

			handlePrivacy := func(c telebot.Context, isPublic bool) error {
				var privacy, command string

				if isPublic {
					privacy = "Public"
					command = "/profile"
				} else {
					privacy = "Private"
					command = "/account"
				}

				entry := &Entry{
					UserID: c.Sender().ID,
					Note: number,
					Text: answer.Text,
				}

				db.Last(&entry)

				entry.IsPublic = true

				db.Save(&entry)

				_, err := b.Edit(c.Message(), localizer.Tr(c.Sender().LanguageCode, "survived-saved", privacy, entry.Note, entry.Text, command))

				return err
			}
				
			b.Handle(&public, func(c telebot.Context) error {
				return handlePrivacy(c, true)
			})

			b.Handle(&private, func(c telebot.Context) error {
				return handlePrivacy(c, false)
			})

			privacyButtons.Inline(privacyButtons.Row(public, private))

			b.Edit(msg, localizer.Tr(c.Sender().LanguageCode, "survived-ask-public"), privacyButtons)
		}

		return nil
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

		return c.Send(localizer.Tr(c.Sender().LanguageCode, "help", users, messageCount, start.Format("02 Jan 06")))
	})

	log.Println("starting bot")
	b.Start()
}

func getRank(start time.Time, rank string, offset int) (int, string) {
	days := int(time.Now().Sub(start).Hours() / 24)
	levels := config.Ranks[strings.ToLower(rank)].Levels
	
	keys := maps.Keys(levels)
	sort.Ints(keys)

	for key, value := range keys {
		if days <= value {
			return value, levels[keys[key + offset]]
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