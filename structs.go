package main

import (
	"gorm.io/gorm"
	"time"
    "strconv"
)

type Rank struct {
	ID     string
	Name   string
	Score  int
	Levels map[int]string
}

type Motivation struct {
	UUID      string `gorm:"primaryKey"`
	Pack      string
	PackPlace int
	ID        string
	Category  string
	Language  string
	Extension string
	Path      string
}

type User struct {
	gorm.Model
	ID       int64 `gorm:"primaryKey"`
	Username string
}

func (u User) Recipient() string {
    return strconv.FormatInt(u.ID, 10)
}

type Journey struct {
	gorm.Model   `yaml:"-"`
	CreatedAtStr string `yaml:"createdat"`
	UserID       int64  `yaml:"-"`
	RankSystem   string
	Start        time.Time
	End          time.Time
	Text         string
}

type Entry struct {
	gorm.Model   `yaml:"-"`
	CreatedAtStr string `yaml:"createdat"`
	UserID       int64  `yaml:"-"`
	IsPublic     bool
	Note         int
	Text         string `gorm:"size:4096"`
}

type Task struct {
	gorm.Model   `yaml:"-"`
	CreatedAtStr string    `yaml:"createdat"`
	UserID       int64     `yaml:"-"`
	ChatID       int64     `yaml:"-"`
	MessageID    int       `yaml:"-"`
	TaskID       int       `yaml:"-"`
	Date         time.Time `gorm:"autoCreateTime"`
	Done         time.Time `gorm:"autoUpdateTime"`
	Text         string
	IsDone       bool
}

type TaskData struct {
	gorm.Model
	Points int
	Task   string
}

type Activity struct {
	CreatedAt time.Time `yaml:"-"`
	Type      string
	Item      interface{}
}
