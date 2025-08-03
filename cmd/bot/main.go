package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/joho/godotenv"
	"github.com/skip2/go-qrcode"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/binary/proto"
	_ "github.com/mattn/go-sqlite3"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types/events"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"whatsapp-transcriber-go/internal/exclusion"
	"whatsapp-transcriber-go/internal/transcription"
)

var log *zap.Logger
var cli *whatsmeow.Client
var exclusionManager *exclusion.Manager
var transcriberService transcription.Transcriber
var transcriptionLanguage string

func main() {
	// Load .env file
	err := godotenv.Load()
	if err != nil {
		fmt.Println("Error loading .env file, assuming environment variables are set.")
	}

	// Setup logging
	config := zap.NewProductionEncoderConfig()
	config.EncodeTime = zapcore.ISO8601TimeEncoder
	fileEncoder := zapcore.NewJSONEncoder(config)
	consoleEncoder := zapcore.NewConsoleEncoder(config)

	logFile, err := os.OpenFile("logs/debug.log", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		panic(fmt.Sprintf("Failed to open log file: %v", err))
	}
	writer := zapcore.AddSync(logFile)

	core := zapcore.NewTee(
		zapcore.NewCore(consoleEncoder, zapcore.AddSync(os.Stdout), zapcore.InfoLevel),
		zapcore.NewCore(fileEncoder, writer, zapcore.DebugLevel),
	)
	log = zap.New(core, zap.AddCaller())
	defer log.Sync()

	log.Info("Starting WhatsApp Audio Transcription Bot...")

	// Initialize Whatsmeow client
	container, err := sqlstore.New(context.Background(), "sqlite3", "file:data/session.db?_foreign_keys=on", nil)
	if err != nil {
		log.Fatal("Failed to create container", zap.Error(err))
	}

	deviceStore, err := container.GetFirstDevice(context.Background())
	if err != nil {
		log.Fatal("Failed to get device from container", zap.Error(err))
	}
	cli = whatsmeow.NewClient(deviceStore, nil)
	cli.AddEventHandler(eventHandler)

	// Initialize exclusion manager
	exclusionManager = exclusion.NewManager("data/exclude.txt", log)

	// Configure transcription service
	groqAPIKey := os.Getenv("GROQ_API_KEY")
	cloudflareAccountID := os.Getenv("CF_ACCOUNT_ID")
	cloudflareAPIKey := os.Getenv("CF_API_KEY")
	transcriptionLanguage = os.Getenv("TRANSCRIPTION_LANGUAGE")
	if transcriptionLanguage == "" {
		transcriptionLanguage = "pt" // Default to Portuguese
	}

	if groqAPIKey != "" {
		transcriberService = transcription.NewGroqTranscriber(groqAPIKey, "whisper-large-v3", log)
		log.Info("Using Groq API for transcription.")
	} else if cloudflareAccountID != "" && cloudflareAPIKey != "" {
		transcriberService = transcription.NewCloudflareAITranscriber(cloudflareAccountID, cloudflareAPIKey, "@cf/openai/whisper", log)
		log.Info("Using Cloudflare AI for transcription.")
	} else {
		log.Fatal("No transcription API keys found. Please set GROQ_API_KEY or CF_ACCOUNT_ID and CF_API_KEY in your .env file.")
	}

	// Load session or login
	if cli.Store.ID == nil {
		// No ID stored, new session
		qrChan, _ := cli.GetQRChannel(context.Background())
		log.Info("Generating QR code...")
		go func() {
			for evt := range qrChan {
				if evt.Event == "code" {
					fmt.Println("QR Code generated! Please scan this with your WhatsApp mobile app:")
					fmt.Println()
					
					// Generate and display visual QR code
					err := qrcode.WriteFile(evt.Code, qrcode.Medium, 256, "qrcode.png")
					if err != nil {
						log.Error("Failed to generate QR code image", zap.Error(err))
						fmt.Printf("Alternatively, you can manually scan this code: %s\n", evt.Code)
					} else {
						fmt.Println("QR code saved as qrcode.png")
						fmt.Println("You can also scan this code from the file:")
						fmt.Println("qrcode.png")
					}
					fmt.Println()
				} else if evt.Event == "timeout" {
					log.Info("QR code timeout, generating new one...")
					fmt.Println("QR code timed out. A new one will be generated...")
				} else {
					log.Info("Login event", zap.Any("event", evt.Event))
				}
			}
		}()
		err = cli.Connect()
		if err != nil {
			log.Fatal("Failed to connect", zap.Error(err))
		}
	} else {
		// Already logged in, just connect
		err = cli.Connect()
		if err != nil {
			log.Fatal("Failed to connect", zap.Error(err))
		}
	}

	// Listen for interrupt signals
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	<-c

	cli.Disconnect()
	log.Info("Disconnected from WhatsApp.")
}

func eventHandler(evt interface{}) {
	switch v := evt.(type) {
	case *events.Connected:
		log.Info("WhatsApp client connected!")
	case *events.Disconnected:
		log.Info("WhatsApp client disconnected!")
	case *events.Message:
		// Filter out group messages
		if v.Info.IsGroup {
			log.Debug("Ignoring group message", zap.String("from", v.Info.Chat.String()))
			return
		}

		senderJID := v.Info.Sender.String()
		// Check if sender is excluded
		if exclusionManager.IsExcluded(senderJID) {
			log.Debug("Ignoring message from excluded sender", zap.String("from", senderJID))
			return
		}

		// Handle administrative commands
		if v.Message.GetConversation() != "" {
			text := v.Message.GetConversation()
			if text == "/exclude" {
				cli.SendMessage(context.Background(), v.Info.Chat, &proto.Message{
					Conversation: &text,
				})
				return
			} else if text == "/include" {
				cli.SendMessage(context.Background(), v.Info.Chat, &proto.Message{
					Conversation: &text,
				})
				return
			} else if len(text) > 9 && text[:9] == "/exclude " {
				numberToExclude := text[9:]
				exclusionManager.Add(numberToExclude)
				cli.SendMessage(context.Background(), v.Info.Chat, &proto.Message{
					Conversation: &[]string{fmt.Sprintf("Number %s added to exclusion list.", numberToExclude)}[0],
				})
				return
			} else if len(text) > 9 && text[:9] == "/include " {
				numberToInclude := text[9:]
				exclusionManager.Remove(numberToInclude)
				cli.SendMessage(context.Background(), v.Info.Chat, &proto.Message{
					Conversation: &[]string{fmt.Sprintf("Number %s removed from exclusion list.", numberToInclude)}[0],
				})
				return
			}
		}

		// Check for audio messages
		if v.Message.GetAudioMessage() != nil {
			log.Info("Received audio message", zap.String("from", senderJID))
			job := transcription.NewJob(cli, v, log, transcriberService, transcriptionLanguage)
			go job.HandleAudioMessage(context.Background()) // Run in a goroutine to avoid blocking event handler
		} else {
			log.Debug("Received non-audio message", zap.String("from", senderJID), zap.String("type", v.Info.Type))
		}
	}
}