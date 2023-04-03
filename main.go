// Copyright 2023 Shift Crypto AG
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"os/signal"
	"runtime/debug"
	"sync"
	"syscall"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api"
)

var (
	cacheFilename  = flag.String("cache", "cache.json", "Filename for the persistent cache")
	configFilename = flag.String("config", "config.json", "Config file. Protect with 0600 as it contains the secret bot token.")
	// If a user posts a message for the first time after this amount of time, we send a message
	// replying to them that warns them of scammers.
	warnAfter = flag.Duration("warnAfter", 14*24*time.Hour, "Warn user when they post a message after this amount of inactivity. Defaults to two weeks.")
)

var buildCommit = func() string {
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, setting := range info.Settings {
			if setting.Key == "vcs.revision" {
				return setting.Value
			}
		}
	}

	return ""
}()

const groupTitleBitBoxEn = "BitBox"
const groupTitleBitBoxDE = "BitBox DE"
const warnMessageDefaultEn = "Do not respond to any direct messages or calls."
const warnMessageDefaultDe = "Antworte nicht auf private Nachrichten oder Anrufe. Betrüger am Werk."

type Config struct {
	BotToken      string
	WarnMessageEn string
	WarnMessageDe string
}

type UserID int
type ChatID int64

type UserData struct {
	LastMessageAt time.Time
}

type ChatData struct {
	Title    string
	UserData map[UserID]*UserData
}

type Data struct {
	ChatData map[ChatID]*ChatData
	changed  bool
	lock     sync.Mutex
}

func (d *Data) save() {
	d.lock.Lock()
	defer d.lock.Unlock()

	if !d.changed {
		log.Println("periodicSave: nothing to do")
		return
	}

	jsonBytes, err := json.Marshal(d)
	d.changed = false
	if err != nil {
		log.Println("could not serialize data")
		return
	}
	if err := ioutil.WriteFile(*cacheFilename, jsonBytes, 0600); err != nil {
		log.Println("could not save data")
		return
	}
	log.Println("cache saved")
}

func (d *Data) periodicSave() {
	for {
		time.Sleep(10 * time.Minute)
		d.save()
	}
}

func process(config *Config, data *Data, bot *tgbotapi.BotAPI, msg *tgbotapi.Message) {
	if msg == nil || msg.Chat == nil {
		return
	}

	switch msg.Chat.Title {
	case "Warntest", groupTitleBitBoxEn, groupTitleBitBoxDE:
	default:
		_, err := bot.LeaveChat(tgbotapi.ChatConfig{ChatID: msg.Chat.ID})
		if err != nil {
			log.Printf("error leaving chat: %v", err)
			return
		}
		log.Printf("left group %v (%v)", msg.Chat.ID, msg.Chat.Title)
		return
	}

	// Bots do not need warnings.
	if msg.From.IsBot {
		log.Println("ignoring msg from bot")
		return
	}
	// Do not warn users who wrote a response to a message, to reduce the noise. For now we
	// assume the primary target of attackers are users who ask a question, which are usually
	// top-level messages.
	if msg.ReplyToMessage != nil {
		return
	}

	chatID := ChatID(msg.Chat.ID)
	userID := UserID(msg.From.ID)

	log.Printf("update: ChatID=%v, ChatTitle=%v\n", msg.Chat.ID, msg.Chat.Title)

	data.lock.Lock()
	defer data.lock.Unlock()

	if _, ok := data.ChatData[chatID]; !ok {
		data.ChatData[chatID] = &ChatData{
			UserData: map[UserID]*UserData{},
		}
	}

	data.ChatData[chatID].Title = msg.Chat.Title

	if _, ok := data.ChatData[chatID].UserData[userID]; !ok {
		data.ChatData[chatID].UserData[userID] = &UserData{}
	}
	userData := data.ChatData[chatID].UserData[userID]
	if time.Since(userData.LastMessageAt) > *warnAfter {
		// If the user hasn't posted in this group in over a month, send a warning message
		warnMessage := config.WarnMessageEn
		if msg.Chat.Title == groupTitleBitBoxDE {
			warnMessage = config.WarnMessageDe
		}
		reply := tgbotapi.NewMessage(int64(chatID), warnMessage)
		reply.ReplyToMessageID = msg.MessageID
		_, err := bot.Send(reply)
		if err != nil {
			log.Printf("error warning user: %v", err)
		} else {
			log.Println("warned user")
		}
	} else {
		log.Println("didn't warn user; already warned before")
	}

	// Update the last post time for the user in this group
	userData.LastMessageAt = time.Now()
	data.changed = true
}

func main() {
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "Build commit: %v\n", buildCommit)
		fmt.Fprintf(flag.CommandLine.Output(), "Usage of %s:\n", os.Args[0])
		flag.PrintDefaults()
	}
	flag.Parse()

	configBytes, err := ioutil.ReadFile(*configFilename)
	if err != nil {
		log.Fatal(err)
	}
	var config Config
	if err := json.Unmarshal(configBytes, &config); err != nil {
		log.Fatal(err)
	}
	if config.WarnMessageEn == "" {
		config.WarnMessageEn = warnMessageDefaultEn
	}
	if config.WarnMessageDe == "" {
		config.WarnMessageDe = warnMessageDefaultDe
	}

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	done := make(chan bool, 1)
	go func() {
		<-sigs
		done <- true
	}()

	bot, err := tgbotapi.NewBotAPI(config.BotToken)
	if err != nil {
		log.Fatal(err)
	}

	// Set up a channel to receive updates
	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60

	updates, err := bot.GetUpdatesChan(u)
	if err != nil {
		log.Fatal(err)
	}

	// Keep track of the last time the user posted in each group
	data := &Data{}

	jsonBytes, err := ioutil.ReadFile(*cacheFilename)
	if err == nil {
		if err := json.Unmarshal(jsonBytes, data); err != nil {
			log.Println("could not load cache.json; ignoring")
			data = &Data{}
		} else {
			log.Println("cache loaded from file")
		}
	}

	if data.ChatData == nil {
		data.ChatData = map[ChatID]*ChatData{}
	}

	go data.periodicSave()

	log.Printf("running; warnAfter=%v\n", *warnAfter)
	for {
		select {
		case update := <-updates:
			process(&config, data, bot, update.Message)
		case <-done:
			fmt.Println("exiting")
			data.save()
			return
		}
	}
}
