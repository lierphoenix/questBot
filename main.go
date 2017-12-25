package main

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"strconv"
	"strings"

	"time"

	"github.com/boltdb/bolt"
	"github.com/go-telegram-bot-api/telegram-bot-api"
	"github.com/robfig/cron"
	"gopkg.in/yaml.v2"
)

const questStartLink = "first"
const questFinishLink = "exit"
const sessionsBucketName = "user_sessions"

const blockTypeUserInput = 1    //Блок ожидания пользовательского ввода
const blockTypeAnswerChoice = 2 //Блок выбора ответа
const blockTypePutStuff = 3     //Блок пополнения снаряжения
const blockTypeCheckStuff = 4   //Блок проверки необходимого сняряжения
const blockTypeShowMessage = 5  //Блок показа сообщения и преход по GoTo

type storyIteration struct {
	Monologue  []string
	Question   string
	Answers    []map[string]string
	Prompt     string
	GoTo       string
	Stuff      string //TODO Stuff - массив
	CheckStuff map[string]string
}

type appConfig struct {
	BotToken      string                    `yaml:"bot_token"`
	Cron          string                    `yaml:"cron"`
	Notifications map[int]map[string]string `yaml:"user_notifications"`
	Env           string                    `yaml:"env"`
}

var bot *tgbotapi.BotAPI
var story map[string]storyIteration
var config appConfig

func init() {
	loadConfig("config.yml")    //TODO in execution parameter
	loadSessions("sessions.db") //TODO in execution parameter

	loadStory()
	checkStory()

	initBot()
}

func main() {
	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60

	updates, err := bot.GetUpdatesChan(u)
	if err != nil {
		log.Panic(err)
	}

	enableUserNotify(config.Cron)

	for update := range updates {
		if update.Message == nil {
			continue
		}

		go proceedMessage(update.Message.Chat.ID, update.Message.Text)
	}
}

func proceedMessage(chatId int64, messageFromUser string) {

	fmt.Println(messageFromUser)

	session := sessions.get(chatId)
	fmt.Printf("%+v\n\n", session)

	if userWantRestart(messageFromUser) {
		session.setPosition(questStartLink)
		session.setWorking(true)

		startStoryPosition := story[questStartLink]
		showMonologue(chatId, startStoryPosition.Monologue)
		askQuestion(chatId, startStoryPosition)

		session.setWorking(false)
	} else {
		if session.IsWorking {
			fmt.Println("Заблокирован ввод пользователя")
			return
		}

		lastStorySubject := story[session.Position]
		currentStorySubject, postback, err := getCurrentPosition(messageFromUser, lastStorySubject)

		if len(err) > 0 {
			redrawLastPosition(chatId, err, lastStorySubject)
			return
		}

		session.setWorking(true) //Заблокировали ввод пользователя

		//Количество переходов по истории без участия пользователя
		for i := 0; i < 3; i++ {
			fmt.Println(session.Stuff)
			if proceedPrompt(messageFromUser, lastStorySubject, session) {
				fmt.Println("Записали в stuff")
			}

			typeOfBlock := getTypeOfBlock(messageFromUser, lastStorySubject, currentStorySubject)
			switch typeOfBlock {
			case blockTypeUserInput:
				showMonologue(chatId, currentStorySubject.Monologue)
				fmt.Println("Ожидание пользовательского ввода")
				askQuestion(chatId, currentStorySubject)

				session.setPosition(postback)
				session.setWorking(false)
				return

			case blockTypeAnswerChoice:
				fmt.Println("Выбор ответа")

				showMonologue(chatId, currentStorySubject.Monologue)
				askQuestion(chatId, currentStorySubject)

				session.setPosition(postback)
				session.setWorking(false)
				return

			case blockTypePutStuff:
				fmt.Println("Берем вещь и идем дальше")
				showMonologue(chatId, currentStorySubject.Monologue)
				proceedPutStuff(&postback, &currentStorySubject, session)

				session.setPosition(postback)
				continue

			case blockTypeCheckStuff:
				fmt.Println("Есть ли нужное барахло")
				showMonologue(chatId, currentStorySubject.Monologue)
				proceedCheckStuff(&postback, &currentStorySubject, session)

				continue

			case blockTypeShowMessage:
				fmt.Println("Зачитал и перешел на вопрос. Переносит question в следующую итерацию")

				session.setPosition(postback)

				postback = currentStorySubject.GoTo
				lastStorySubject = currentStorySubject
				currentStorySubject = story[postback]

				mergeStoryBlocks(&currentStorySubject, &lastStorySubject)
				continue
			}
		}
	}
}

func proceedCheckStuff(postback *string, currentStoryBlock *storyIteration, sess *UserSession) {
	userStuff := sess.Stuff

	for item, failGoTo := range currentStoryBlock.CheckStuff {
		_, stuffExist := userStuff[item]
		if !stuffExist {
			*postback = failGoTo
			*currentStoryBlock = story[*postback]
			fmt.Println("fail card goto")
			return
		}
	}

	sess.setPosition(*postback)
	*postback = currentStoryBlock.GoTo
	*currentStoryBlock = story[*postback]
	fmt.Println("success card goto")
}

func getTypeOfBlock(messageFromUser string, lastStoryBlock storyIteration, currentStoryBlock storyIteration) int {
	if len(currentStoryBlock.GoTo) == 0 {
		if len(currentStoryBlock.Answers) > 0 && len(currentStoryBlock.Question) > 0 { //Выбор готового решения
			return blockTypeAnswerChoice
		}
	} else {
		if len(currentStoryBlock.Prompt) > 0 { // Ожидание ввода от пользователя
			return blockTypeUserInput
		} else if len(currentStoryBlock.Stuff) > 0 { //Ложим что-то в заплечный мешок
			return blockTypePutStuff
		} else if len(currentStoryBlock.CheckStuff) > 0 { //Проверка сняряги
			return blockTypeCheckStuff
		} else if len(currentStoryBlock.Monologue) > 0 { // Зачитывем монолог и переходим
			return blockTypeShowMessage
		}
	}

	log.Printf("Блок с неизвестным назначением. %+v\n", currentStoryBlock)
	os.Exit(1)
	return 1
}

func userWantRestart(message string) bool {
	return message == "/start" || message == "start" || message == "/logout" || message == "logout" || message == "/stop"
}

func proceedPutStuff(postback *string, currentStoryObject *storyIteration, session *UserSession) {
	if len(currentStoryObject.Stuff) > 0 && len(currentStoryObject.GoTo) > 0 {
		//Берем stuff и сдвигаем вперед сессию
		if nil == session.Stuff {
			session.Stuff = make(map[string]string)
		}

		session.addStuff(currentStoryObject.Stuff, "1")

		*postback = currentStoryObject.GoTo
		*currentStoryObject = story[currentStoryObject.GoTo]
	}
}

func proceedPrompt(userMessage string, lastStorySubject storyIteration, session *UserSession) bool {
	if len(lastStorySubject.Prompt) > 0 {
		if nil == session.Stuff {
			session.Stuff = make(map[string]string)
		}

		session.addStuff(lastStorySubject.Prompt, userMessage)
		return true
	}

	return false
}

func getCurrentPosition(messageFromUser string, lastStorySubject storyIteration) (storyIteration, string, string) {
	if len(lastStorySubject.Answers) > 0 {
		//Проверяем, если предыдущая итерация закончилась выбором ответа
		var postback string
		for _, answer := range lastStorySubject.Answers {
			if answer["title"] == messageFromUser {
				postback = answer["postback"]
				break
			}
		}

		if len(postback) > 0 {
			storyItem, ok := story[postback]
			if ok {
				return storyItem, postback, ""
			}
		}

		return storyIteration{}, "", "Я вас не понимаю."

	} else if len(lastStorySubject.Prompt) > 0 && len(lastStorySubject.GoTo) > 0 {
		//Проверяем, если предыдущая итерация закончилась запросом пользовательского ввода
		//TODO Проверка ввода пользователя на ругательства

		currentStorySubject, ok := story[lastStorySubject.GoTo]
		if !ok {
			return storyIteration{}, "", "Я вас не понимаю4."
		}

		return currentStorySubject, lastStorySubject.GoTo, ""
	}

	log.Println("Неизвестно, что делать дальше")
	fmt.Println(lastStorySubject)
	fmt.Println(messageFromUser)
	return storyIteration{}, "", "Alert! Error! Unknown user reaction"
}

func showMonologue(chatId int64, monologueCollection []string) {
	for _, message := range monologueCollection {
		var msg tgbotapi.Chattable

		if strings.Contains(message, "images") {
			msg = tgbotapi.NewPhotoUpload(chatId, message)
		} else if strings.Contains(message, "sound") {
			msg = tgbotapi.NewAudioUpload(chatId, message)
		} else {
			msg = generateTextMessage(chatId, message)
		}
		bot.Send(msg)

		time.Sleep(time.Millisecond * 300)
	}
}

func askQuestion(chatId int64, currentStoryPosition storyIteration) {
	msg := generateTextMessage(chatId, currentStoryPosition.Question)
	fmt.Println("Отправляем сообщение пользователю")

	if len(currentStoryPosition.Answers) > 0 { // Выбор из готового ответа

		markup := tgbotapi.NewReplyKeyboard()
		for _, button := range currentStoryPosition.Answers {
			row := []tgbotapi.KeyboardButton{{
				Text:            button["title"],
				RequestContact:  false,
				RequestLocation: false,
			}}
			markup.Keyboard = append(markup.Keyboard, row)
		}

		markup.OneTimeKeyboard = true
		msg.ReplyMarkup = &markup
	}

	bot.Send(msg)
}

func redrawLastPosition(chatId int64, message string, lastStorySubject storyIteration) {
	//TODO половина кода повторяется с askQuestion - вынести общее в другую функцию
	msg := generateTextMessage(chatId, message)

	markup := tgbotapi.NewReplyKeyboard()

	for _, button := range lastStorySubject.Answers {
		row := []tgbotapi.KeyboardButton{{
			Text:            button["title"],
			RequestContact:  false,
			RequestLocation: false,
		}}
		markup.Keyboard = append(markup.Keyboard, row)
	}

	markup.OneTimeKeyboard = true
	msg.ReplyMarkup = &markup

	bot.Send(msg)
}

func generateTextMessage(chatId int64, message string) tgbotapi.MessageConfig {
	session := sessions.get(chatId)

	for stuffKey, stuffItem := range session.Stuff {
		message = strings.Replace(message, "["+stuffKey+"]", stuffItem, -1)
	}

	return tgbotapi.NewMessage(chatId, message)
}

func loadStory() {
	//bot.Debug = true
	file, err := ioutil.ReadFile("./story.json")
	if err != nil {
		fmt.Printf("File error: %v\n", err)
		os.Exit(1)
	}

	json.Unmarshal(file, &story)

	log.Printf("Story is loaded")
}

func checkStory() {
	//Проверка мапы на корректность
	//TODO - не должно быть в отдной и той же итерации Answers и Prompt
	//TODO - если есть Prompt, должен быть и GoTo
	//TODO - что всем postback соответствуют пункты из истории
}

func initBot() {
	err := error(nil)
	bot, err = tgbotapi.NewBotAPI(config.BotToken)
	if err != nil {
		log.Panic(err)
	}

	log.Printf("Authorized on account %s", bot.Self.UserName)
}

func loadConfig(fileName string) {
	file, err := ioutil.ReadFile(fileName)
	if err != nil {
		log.Printf("Error opening %s: #%v ", fileName, err)
		os.Exit(1)
	}

	err = yaml.Unmarshal(file, &config)
	if err != nil {
		log.Fatalf("Error reading %s: %v", fileName, err)
		os.Exit(1)
	}

	for taskId, task := range config.Notifications {
		delay, ok := task["silence_time"]
		if !ok {
			log.Println("Config error: not found silence_time", taskId)
			os.Exit(1)
		}
		if len(delay) == 0 {
			log.Println("Config error: invalid silence_time", taskId)
			os.Exit(1)
		}

		message, ok := task["message"]
		if !ok {
			log.Println("Config error: not found message", taskId)
			os.Exit(1)
		}
		if len(message) < 3 {
			log.Println("Config error: invalid message", taskId)
			os.Exit(1)
		}
	}

	fmt.Printf("%+v\n\n", config)
}

func mergeStoryBlocks(currentStorySubject *storyIteration, lastStorySubject *storyIteration) {
	if len(currentStorySubject.Question) == 0 {
		if len(currentStorySubject.Monologue) == 0 {
			//Если нет монолога и вопроса - последний из монолога переносим в вопрос.
			//Остальное - в монолог
			monologue := lastStorySubject.Monologue
			question := ""

			if len(lastStorySubject.Monologue) > 1 {
				question = monologue[len(monologue)-1]
				monologue = monologue[:len(monologue)-1]
			} else if len(lastStorySubject.Monologue) == 1 {
				question = monologue[len(monologue)-1]
				monologue = []string{}
			} else {
				fmt.Println("Недостижимое условие!!!")
				os.Exit(1)
			}

			currentStorySubject.Question = question
			currentStorySubject.Monologue = monologue
		} else {
			//Если нет вопроса и есть монолог - мержим монологи
			currentStorySubject.Monologue = append(lastStorySubject.Monologue, currentStorySubject.Monologue...)
		}
	} else {
		if len(currentStorySubject.Monologue) == 0 {
			//Если есть вопрос и нет монолога - переносим монолог
			currentStorySubject.Monologue = lastStorySubject.Monologue
		} else {
			//Если есть вопрос и есть монолог - мержим монологи. Вопрос не трогаем
			currentStorySubject.Monologue = append(lastStorySubject.Monologue, currentStorySubject.Monologue...)
		}
	}
}

func enableUserNotify(crontime string) {
	//Напоминания о забытом боте для пользователя
	fmt.Println("Поставили крон", crontime)
	c := cron.New()
	c.AddFunc(crontime, func() {
		log.Println("Запустили крон")
		var sessionsToUpdate []int64
		//Ищем в БД подходяще чаты для напоминалок пользователей
		err := db.View(func(tx *bolt.Tx) error {
			b := tx.Bucket([]byte(sessionsBucketName))
			c := b.Cursor()

			for k, v := c.First(); k != nil; k, v = c.Next() {
				chatId, err := strconv.ParseInt(string(k), 10, 64) //TODO нормальная конвертация в int64
				if nil != err {
					return err
				}

				session := new(UserSession)
				err = json.Unmarshal(v, &session)

				if nil != err {
					return err
				}

				fmt.Printf("key=%v value=%v\n", chatId, *session)

				if session.Position == questStartLink || session.Position == questFinishLink {
					fmt.Println("Не отсылаем ничего. Пользователь на нейтральной позиции")

					continue
				}

				realDiff := time.Since(session.UpdatedAt).Hours()

				notify, ok := config.Notifications[session.NotifyCount]
				if ok {
					fmt.Println("Найдена инструкция в конфиге для уведомления")

					needDiff, _ := strconv.ParseFloat(notify["silence_time"], 64)
					if realDiff >= needDiff {
						fmt.Println("Прошло нужное кол-во времени")

						sessions.set(session.UserId, *session)
						sessionsToUpdate = append(sessionsToUpdate, session.UserId)

						//Отправляем сообщение с текущей позицией
						currentPosition := story[session.Position]
						currentPosition.Monologue = []string{}
						currentPosition.Question = notify["message"]
						askQuestion(session.UserId, currentPosition)
					}
				}
			}

			return nil
		})

		for _, sessionId := range sessionsToUpdate {
			sessions.get(sessionId).increaseNotifyCount()
		}

		fmt.Println(err)
	})

	c.Start()
}
