package main

import(
	"time"
	"gorm.io/gorm"
)

type Config struct {
	Token 		string
	Timeout 	int
	SetCommands bool
	Database 	string
	Commands 	map[string]string
	Ranks 		map[string]Rank
	Tasks 		map[int]TaskData
}

type Rank struct {
	ID 			string
	Name 		string
	Score 		int
	Levels 		map[int]string
}

type TaskData struct {
	Points 		int
	Task 		string
}

type Journey struct {
	gorm.Model
	UserID 				int64
	RankSystem			string
	Start				time.Time
	End					time.Time
	Text				string
}

type Entry struct {
	gorm.Model
	UserID 				int64
	IsPublic 			bool
	Note				int
	Text				string 		`gorm:"size:4096"`
}

type Task struct {
	gorm.Model
	UserID				int64
	TaskID 				int
	IsDone 				bool
}