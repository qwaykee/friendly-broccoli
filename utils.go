package main

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