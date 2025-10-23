// 文件: main.go
package main

import (
	"fmt"
	"k8s-cicd/internal/api"
	"k8s-cicd/internal/config"
	"k8s-cicd/internal/storage"
	"log"
	"net/http"
	"time"

	"github.com/go-co-op/gocron/v2"
	"github.com/go-co-op/gocron/v2/events"  // ✅ 正确导入 events 子包
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"go.mongodb.org/mongo-driver/bson"
)

func main() {
	// 1. 加载配置
	cfg, err := config.LoadConfig()
	if err != nil {
		log.Fatalf("加载配置失败: %v", err)
	}

	// 2. 初始化主 MongoDB
	mongoStorage, err := storage.NewMongoStorage(cfg.MongoURI, cfg.TTLH)
	if err != nil {
		log.Fatalf("初始化 MongoDB 失败: %v", err)
	}
	log.Println("✅ 主 MongoDB 初始化完成")

	// 3. 初始化统计 MongoDB
	statsStorage, err := storage.NewStatsStorage(cfg.MongoURI)
	if err != nil {
		log.Fatalf("初始化统计 MongoDB 失败: %v", err)
	}
	log.Println("✅ 统计 MongoDB 初始化完成")

	// 4. 初始化 Telegram Bot
	bot, err := tgbotapi.NewBotAPI(cfg.TelegramToken)
	if err != nil {
		log.Printf("⚠️  Telegram Bot 初始化失败: %v", err)
		bot = nil
	} else {
		log.Println("✅ Telegram Bot 初始化完成")
	}

	// 5. 初始化 API 服务
	apiServer := api.NewServer(mongoStorage, statsStorage, cfg)

	// 6. 启动 HTTP 服务
	addr := fmt.Sprintf(":%d", cfg.Port)
	go func() {
		log.Printf("🚀 启动 HTTP 服务于 %s", addr)
		if err := http.ListenAndServe(addr, apiServer.Router); err != nil {
			log.Fatalf("HTTP 服务启动失败: %v", err)
		}
	}()

	// 7. 初始化任务调度器
	initScheduler(statsStorage, bot, cfg.TelegramChatID)

	// 8. 阻塞主线程
	log.Println("🎉 系统启动完成，等待任务...")
	select {}
}

// initScheduler 初始化任务调度
func initScheduler(stats *storage.StatsStorage, bot *tgbotapi.BotAPI, chatID int64) {
	s, err := gocron.NewScheduler(
		gocron.WithLocation(time.UTC),
		gocron.WithLogger(gocron.VerboseLogger), // ✅ 使用 events 包的日志
	)
	if err != nil {
		log.Fatalf("初始化调度器失败: %v", err)
	}

	// 每日报告：每天 00:00
	dailyJob, err := s.NewJob(
		gocron.DailyAt("00:00"),
		gocron.NewTaskWithData(func() { sendDailyReport(stats, bot, chatID) }),
	)
	if err != nil {
		log.Printf("⚠️ 每日报告调度失败: %v", err)
	} else {
		// ✅ 使用 events 监听任务完成
		events.ListenJobFinish(dailyJob, func(e events.Event) {
			log.Printf("✅ 每日报告已发送: %v", e.Job().ID())
		})
	}

	// 每月报告：每月3号 00:00
	monthlyJob, err := s.NewJob(
		gocron.MonthlyAt(gocron.Day(3), "00:00"),
		gocron.NewTaskWithData(func() { sendMonthlyReport(stats, bot, chatID, s) }),
	)
	if err != nil {
		log.Printf("⚠️ 每月报告调度失败: %v", err)
	} else {
		// ✅ 使用 events 监听任务完成
		events.ListenJobFinish(monthlyJob, func(e events.Event) {
			log.Printf("✅ 月报已发送: %v", e.Job().ID())
		})
	}

	s.Start()
	log.Println("✅ 任务调度器启动")
}

// sendDailyReport 发送每日报告
func sendDailyReport(stats *storage.StatsStorage, bot *tgbotapi.BotAPI, chatID int64) {
	now := time.Now().UTC()
	today := now.Truncate(24 * time.Hour)
	tomorrow := today.Add(24 * time.Hour)
	match := bson.D{{"timestamp", bson.D{{"$gte", today}, {"$lt", tomorrow}}}}

	results, err := stats.GetStats(match)
	if err != nil {
		log.Printf("获取每日统计失败: %v", err)
		return
	}

	text := "📊 *Daily Deploy Report*\n\n"
	if len(results) == 0 {
		text += "今日无部署记录"
	} else {
		for _, r := range results {
			text += fmt.Sprintf("• *%s* - %s: `%d` versions\n", r.Service, r.Environment, r.Count)
		}
	}

	sendTelegramMessage(bot, chatID, text, "MarkdownV2")
}

// sendMonthlyReport 发送每月报告
func sendMonthlyReport(stats *storage.StatsStorage, bot *tgbotapi.BotAPI, chatID int64, scheduler *gocron.Scheduler) {
	now := time.Now().UTC()
	prevMonthFirst := time.Date(now.Year(), now.Month()-1, 1, 0, 0, 0, 0, time.UTC)
	nextMonthFirst := prevMonthFirst.AddDate(0, 1, 0)

	match := bson.D{{"timestamp", bson.D{{"$gte", prevMonthFirst}, {"$lt", nextMonthFirst}}}}

	results, err := stats.GetStats(match)
	if err != nil {
		log.Printf("获取每月统计失败: %v", err)
		return
	}

	text := fmt.Sprintf("📈 *%s Monthly Deploy Report*\n\n", prevMonthFirst.Format("Jan 2006"))
	text += "| Service | Environment | Versions |\n"
	text += "|---------|-------------|----------|\n"

	total := 0
	for _, r := range results {
		text += fmt.Sprintf("| %s | %s | `%d` |\n", r.Service, r.Environment, r.Count)
		total += r.Count
	}
	text += fmt.Sprintf("\n**Total Versions Deployed: `%d`**", total)

	sendTelegramMessage(bot, chatID, text, "MarkdownV2")

	// 7天后删除上月数据
	cleanupJob, err := scheduler.NewJob(
		gocron.DurationJob(7*24*time.Hour),
		gocron.NewTask(func() {
			if err := stats.DeleteMonthData(prevMonthFirst, nextMonthFirst); err != nil {
				log.Printf("删除上月数据失败: %v", err)
			} else {
				log.Printf("✅ 上月数据删除成功: %s", prevMonthFirst.Format("Jan 2006"))
			}
		}),
	)
	if err != nil {
		log.Printf("⚠️ 调度删除任务失败: %v", err)
	} else {
		// ✅ 使用 events 监听清理任务
		events.ListenJobFinish(cleanupJob, func(e events.Event) {
			log.Printf("✅ 数据清理完成: %v", e.Job().ID())
		})
	}
}

// sendTelegramMessage 发送 Telegram 消息
func sendTelegramMessage(bot *tgbotapi.BotAPI, chatID int64, text, parseMode string) {
	if bot == nil || chatID == 0 {
		log.Printf("跳过 Telegram 发送: Bot 或 ChatID 未配置")
		return
	}

	if text == "" {
		return
	}

	msg := tgbotapi.NewMessage(chatID, text)
	msg.ParseMode = parseMode
	_, err := bot.Send(msg)
	if err != nil {
		log.Printf("发送 Telegram 消息失败: %v", err)
	} else {
		log.Println("✅ Telegram 消息发送成功")
	}
}