package main

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/patrickmn/go-cache"
	tgbotapi "gopkg.in/telegram-bot-api.v4"
)

type MessagePipeConfig struct {
	Telegram struct {
		Token  string `json:"Token"`
		ChatID int64  `json:"ChatID"`
	} `json:"Telegram"`
	Vkontakte struct {
		Token     string `json:"Token"`
		ChatID    int64  `json:"ChatID"`
		PeerID    string
		CurrentID string
	} `json:"Vkontakte"`
}

type LongPollInfo struct {
	Response struct {
		Key    string `json:"key"`
		Server string `json:"server"`
		TS     int    `json:"ts"`
	} `json:"response"`
}

type LongPollData struct {
	TS      int             `json:"ts"`
	Updates [][]interface{} `json:"updates"`
}

type UserInfoResponse struct {
	ID        int    `json:"id"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
}

type UserInfo struct {
	Response []UserInfoResponse `json:"response"`
}

type MessageAttachments struct {
	Response struct {
		Count int `json:"count"`
		Items []struct {
			Attachments []struct {
				Type  string `json:"type"`
				Photo struct {
					AccessKey string `json:"access_key"`
				} `json:"photo"`
			} `json:"attachments"`
		} `json:"items"`
	} `json:"response"`
}

type PhotoAttachment struct {
	Response []struct {
		Sizes []struct {
			Src string `json:"src"`
		} `json:"sizes"`
	} `json:"response"`
}

const (
	VK_API_URL     = "https://api.vk.com/method/"
	VK_API_VERSION = "5.75"
)

var (
	Config                 MessagePipeConfig
	TelegramBot            *tgbotapi.BotAPI
	VkontakteNicknameCache *cache.Cache
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)

	ConfigData, err := ioutil.ReadFile("config.json")
	if err != nil {
		log.Panicln(err)
	}

	err = json.Unmarshal(ConfigData, &Config)
	if err != nil {
		log.Panicln(err)
	}

	VkontakteNicknameCache = cache.New(5*time.Minute, 10*time.Minute)

	Config.Vkontakte.PeerID = strconv.FormatInt(Config.Vkontakte.ChatID+2000000000, 10)
	Config.Vkontakte.CurrentID = strconv.Itoa(GetUserVkontakte("").Response[0].ID)

	// Telegram
	TelegramBot, err = tgbotapi.NewBotAPI(Config.Telegram.Token)
	if err != nil {
		log.Panicln(err)
	}
	TelegramBot.Debug = false

	go TgToVk()
	go VkToTg()

	stop := make(chan int)
	<-stop
}

func TgToVk() {
	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60

	updates, err := TelegramBot.GetUpdatesChan(u)
	if err != nil {
		log.Panicln(err)
	}

	for update := range updates {
		if update.Message == nil {
			continue
		}

		if update.Message.Chat.ID != Config.Telegram.ChatID {
			continue
		}

		name := update.Message.From.FirstName + " " + update.Message.From.LastName
		message := name + ":\n" + update.Message.Text

		params := map[string]string{
			"message": message,
			"peer_id": Config.Vkontakte.PeerID,
		}

		ResponseData, err := RequestVkontakte("messages.send", params)
		if err != nil {
			log.Println(err, "Response:", string(ResponseData[:]))
		}
	}
}

func VkToTg() {
	LongPollResponse, err := RequestVkontakte("messages.getLongPollServer", nil)
	if err != nil {
		log.Panicln(err)
	}

	var LongPoll LongPollInfo
	err = json.Unmarshal(LongPollResponse, &LongPoll)
	if err != nil {
		log.Panicln(err)
	}

	LongPollAddress := fmt.Sprintf("https://%s?act=a_check&key=%s&wait=25&mode=2&version=3&ts=",
		LongPoll.Response.Server,
		LongPoll.Response.Key)

	TS := LongPoll.Response.TS

	for {
		ResponseData, err := Request(LongPollAddress + strconv.Itoa(TS))
		if err != nil {
			log.Println(err)
		}

		var Response LongPollData
		err = json.Unmarshal(ResponseData, &Response)
		if err != nil {
			log.Println(err, "Response:", string(ResponseData[:]))
		}

		TS = Response.TS

		// log.Print("all ", string(ResponseData))

		for i := 0; i < len(Response.Updates); i++ {
			update := Response.Updates[i]

			switch update[0] {
			case 4.0:
				ChatID := strconv.FormatFloat(update[3].(float64), 'f', -1, 64)
				if Config.Vkontakte.PeerID != ChatID {
					continue
				}
				// log.Print("-> ", string(ResponseData))

				From, FromExist := update[6].(map[string]interface{})["from"]
				if !FromExist || From.(string) == Config.Vkontakte.CurrentID {
					continue
				}

				MessageID := strconv.FormatFloat(update[1].(float64), 'f', -1, 64)
				Text := update[5].(string)
				User := GetUserVkontakte(From.(string))

				Name := "*" + User.Response[0].FirstName + " " + User.Response[0].LastName + "*" + ":\n"
				Message := Name + Text

				_, AttachmentsExist := update[7].(map[string]interface{})["attach1"]
				if AttachmentsExist {
					AttachmentsCount := len(update[7].(map[string]interface{})) / 2

					for j := 0; j < AttachmentsCount; j++ {
						AttachmentsID := strconv.Itoa(j + 1)

						Attachment, _ := update[7].(map[string]interface{})["attach"+AttachmentsID]
						if update[7].(map[string]interface{})["attach"+AttachmentsID+"_type"] == "photo" {
							MessageDataParameters := map[string]string{
								"message_ids": MessageID,
							}
							MessageData, err := RequestVkontakte("messages.getById", MessageDataParameters)
							if err != nil {
								log.Println(err)
							}

							var MessageInfo MessageAttachments
							err = json.Unmarshal(MessageData, &MessageInfo)
							if err != nil {
								log.Println(err, "Response:", string(MessageData[:]))
							}

							Secret := MessageInfo.Response.Items[0].Attachments[j].Photo.AccessKey

							PhotoParameters := map[string]string{
								"photos":      Attachment.(string) + "_" + Secret,
								"photo_sizes": "1",
							}
							PhotoInfoData, err := RequestVkontakte("photos.getById", PhotoParameters)
							if err != nil {
								log.Println(err)
							}

							var Photo PhotoAttachment
							err = json.Unmarshal(PhotoInfoData, &Photo)
							if err != nil {
								log.Println(err, "Response:", string(PhotoInfoData[:]))
							}

							// log.Print("photo ", string(PhotoInfoData))
							// log.Println(Photo.Response[0].Sizes[len(Photo.Response[0].Sizes)-1].Src)

							msg := tgbotapi.NewPhotoUpload(Config.Telegram.ChatID, Name)
							msg.FileID = Photo.Response[0].Sizes[len(Photo.Response[0].Sizes)-1].Src
							msg.UseExisting = true
							TelegramBot.Send(msg)
						}

					}
				}

				TelegramMessage := tgbotapi.NewMessage(Config.Telegram.ChatID, Message)
				TelegramMessage.ParseMode = "markdown"

				TelegramBot.Send(TelegramMessage)
			}
		}
	}
}

func Request(url string) (body []byte, err error) {
	response, err := http.Get(url)
	if err != nil {
		return nil, err
	}

	defer response.Body.Close()

	body, err = ioutil.ReadAll(response.Body)
	if err != nil {
		return nil, err
	}

	return body, nil
}

func RequestVkontakte(method string, parameters map[string]string) ([]byte, error) {
	u, err := url.Parse(VK_API_URL + method)
	if err != nil {
		return nil, err
	}

	query := url.Values{}
	for k, v := range parameters {
		query.Set(k, v)
	}

	query.Set("access_token", Config.Vkontakte.Token)
	query.Set("v", VK_API_VERSION)

	u.RawQuery = query.Encode()

	return Request(u.String())
}

func GetUserVkontakte(id string) (User UserInfo) {
	FirstName, FirstNameExist := VkontakteNicknameCache.Get(id + "_first")
	LastName, LastNameExist := VkontakteNicknameCache.Get(id + "_last")
	if FirstNameExist && LastNameExist {
		CacheID, _ := strconv.Atoi(id)

		Data := UserInfoResponse{
			FirstName: FirstName.(string),
			LastName:  LastName.(string),
			ID:        CacheID,
		}
		User.Response = append(User.Response, Data)

		return
	}

	UserParameters := map[string]string{}

	if id != "" {
		UserParameters["user_ids"] = id
	}

	UserData, err := RequestVkontakte("users.get", UserParameters)
	if err != nil {
		log.Println(err)
	}

	err = json.Unmarshal(UserData, &User)
	if err != nil {
		log.Println(err, "Response:", string(UserData[:]))
	}

	VkontakteNicknameCache.Set(id+"_first", User.Response[0].FirstName, cache.DefaultExpiration)
	VkontakteNicknameCache.Set(id+"_last", User.Response[0].LastName, cache.DefaultExpiration)

	return
}
