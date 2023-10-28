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
		log.Fatal(err)
	}

	localizer, err = i18n.New(loader, "en-US", "fr-FR")
	if err != nil {
		log.Fatal(err)
	}

	// initialize database
	db, err = gorm.Open(sqlite.Open(config.Database), &gorm.Config{PrepareStmt: true})
	if err != nil {
		log.Fatal(err)
	}

	db.AutoMigrate(&Journey{}, &Entry{}, &Task{})

	// create bot and set commands
	b, err = telebot.NewBot(telebot.Settings{
		Token:  config.Token,
		Poller: &telebot.LongPoller{Timeout: time.Duration(config.Timeout) * time.Second},
		ParseMode: telebot.ModeMarkdown,
	})
	if err != nil {
		log.Fatal(err)
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
			log.Fatal(err)
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
		log.Fatal(err)
	}
}

func main() {
	// make ranks buttons
	var ranksButtons telebot.ReplyMarkup
	var buttons []telebot.Btn

	for _, option := range config.Ranks {
	    buttons = append(buttons, ranksButtons.Data(option.Name, "new:" + option.Name))
	}

	ranksButtons.Inline(ranksButtons.Split(2, buttons)...)

	// Handle message count
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
		if r := db.Find(&Journey{UserID: c.Sender().ID}); r.RowsAffected > 0 {
			return c.Send(localizer.Tr(c.Sender().LanguageCode, "new-already-running-journey"))
		}

		msg, answer, _ := i.Listen(cauliflower.Parameters{
			Context: c,
			Message: localizer.Tr(c.Sender().LanguageCode, "new-ask-streak"),
		})

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

		_, err = b.Edit(msg, text, &ranksButtons)
		return err
	})

	b.Handle("/check", func(c telebot.Context) error {
		now := time.Now()
		midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())

		if r := db.Find(&Entry{}).Where("created_at BETWEEN ? AND ?", midnight, now); r.RowsAffected >= 3 {
			return c.Send(localizer.Tr(c.Sender().LanguageCode, "check-already-checked-in"))
		}

		checkInButtons := b.NewMarkup()

		yes := checkInButtons.Data(localizer.Tr(c.Sender().LanguageCode, "check-relapsebtn-yes"), "relapse:yes")
		no := checkInButtons.Data(localizer.Tr(c.Sender().LanguageCode, "check-relapsebtn-no"), "relapse:no")

		checkInButtons.Inline(checkInButtons.Split(2, []telebot.Btn{yes, no})...)

		return c.Send("Welcome back, did you relapse today?", &checkInButtons)
	})

	b.Handle(telebot.OnCallback, func(c telebot.Context) error {
		callback := c.Callback()
		data := strings.Split(callback.Data, ":")

		switch strings.TrimSpace(data[0]) {
		case "new":
			journey := &Journey{
				UserID: c.Sender().ID,
				End: time.Time{},
			}

			db.Last(&journey)

			journey.RankSystem = data[1]

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

			_, err := b.Edit(callback.Message, text)
			return err
		case "relapse":
			if data[1] == "yes" {
				msg, answer, _ := i.Listen(cauliflower.Parameters{
					Context: c,
					Message: localizer.Tr(c.Sender().LanguageCode, "check-ask-relapsed"),
				})

				
			}
			return nil
		default:
			return c.Send(localizer.Tr(c.Sender().LanguageCode, "err-callback"))
		}
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