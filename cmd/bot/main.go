package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/joho/godotenv"
	_ "github.com/mattn/go-sqlite3"
	"github.com/skip2/go-qrcode"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types/events"

	//	"image/color" // Added for QR code terminal display
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
	// Parse command line flags
	var backendFlag string
	var enableLogging bool
	flag.StringVar(&backendFlag, "backend", "", "Transcription backend to use (groq|cloudflare)")
	flag.BoolVar(&enableLogging, "log", false, "Enable logging to file")
	flag.Parse()

	// Load .env file
	err := godotenv.Load()
	if err != nil {
		fmt.Println("Error loading .env file, assuming environment variables are set.")
	}

	// Setup logging
	config := zap.NewProductionEncoderConfig()
	config.EncodeTime = zapcore.ISO8601TimeEncoder
	consoleEncoder := zapcore.NewConsoleEncoder(config)

	var core zapcore.Core

	if enableLogging {
		// Create logs directory if it doesn't exist
		if err := os.MkdirAll("logs", 0755); err != nil {
			panic(fmt.Sprintf("Failed to create logs directory: %v", err))
		}

		fileEncoder := zapcore.NewJSONEncoder(config)
		logFile, err := os.OpenFile("logs/debug.log", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			panic(fmt.Sprintf("Failed to open log file: %v", err))
		}
		writer := zapcore.AddSync(logFile)

		core = zapcore.NewTee(
			zapcore.NewCore(consoleEncoder, zapcore.AddSync(os.Stdout), zapcore.DebugLevel),
			zapcore.NewCore(fileEncoder, writer, zapcore.DebugLevel),
		)
	} else {
		core = zapcore.NewCore(consoleEncoder, zapcore.AddSync(os.Stdout), zapcore.DebugLevel)
	}

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
	cloudflareModel := os.Getenv("CF_MODEL")
	transcriptionLanguage = os.Getenv("TRANSCRIPTION_LANGUAGE")
	if transcriptionLanguage == "" {
		transcriptionLanguage = "pt" // Default to Portuguese
	}

	// Validate backend flag if provided
	if backendFlag != "" && backendFlag != "groq" && backendFlag != "cloudflare" {
		log.Fatal("Invalid backend specified. Valid options are: groq, cloudflare")
	}

	// Configure transcription service (command line flag takes precedence)
	if backendFlag == "groq" || (backendFlag == "" && groqAPIKey != "") {
		if groqAPIKey == "" {
			log.Fatal("Groq API key not found in environment variables")
		}
		transcriberService = transcription.NewGroqTranscriber(groqAPIKey, "whisper-large-v3", log)
		backendSource := "command line flag"
		if backendFlag == "" {
			backendSource = "environment variable"
		}
		log.Info("Using Groq API for transcription.", zap.String("source", backendSource))
	} else if backendFlag == "cloudflare" || (backendFlag == "" && cloudflareAccountID != "" && cloudflareAPIKey != "") {
		if cloudflareAccountID == "" || cloudflareAPIKey == "" {
			log.Fatal("Cloudflare credentials not found in environment variables")
		}
		model := cloudflareModel
		if model == "" {
			model = "@cf/openai/whisper-large-v3-turbo" // Fallback model
		}
		transcriberService = transcription.NewCloudflareAITranscriber(cloudflareAccountID, cloudflareAPIKey, model, log)
		backendSource := "command line flag"
		if backendFlag == "" {
			backendSource = "environment variable"
		}
		log.Info("Using Cloudflare AI for transcription.", zap.String("source", backendSource))
	} else {
		log.Fatal("No transcription backend configured. Please set --backend flag or environment variables (GROQ_API_KEY or CF_ACCOUNT_ID+CF_API_KEY)")
	}

	// Load session or login
	if cli.Store.ID == nil {
		// No ID stored, new session
		qrChan, _ := cli.GetQRChannel(context.Background())
		log.Info("Generating QR code...")
		go func() {
			for evt := range qrChan {
				switch evt.Event {
				case "code":
					fmt.Println("QR Code generated! Please scan this with your WhatsApp mobile app:")
					fmt.Println()

					// Generate and display visual QR code in terminal
					err := printQRCodeToTerminal(evt.Code)
					if err != nil {
						log.Error("Failed to display QR code in terminal", zap.Error(err))
						fmt.Printf("Alternatively, you can manually scan this code: %s\n", evt.Code)
						// Fallback to saving file if terminal display fails
						err := qrcode.WriteFile(evt.Code, qrcode.Medium, 256, "qrcode.png")
						if err != nil {
							log.Error("Failed to generate QR code image fallback", zap.Error(err))
						} else {
							fmt.Println("QR code saved as qrcode.png (fallback)")
						}
					}
					fmt.Println()
					qrcode.WriteFile(evt.Code, qrcode.Medium, 256, "qrcode.png")
					fmt.Println()
				case "timeout":
					log.Info("QR code timeout, generating new one...")
					fmt.Println("QR code timed out. A new one will be generated...")
				default:
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

// printQRCodeToTerminal generates a QR code and prints it to the terminal as ASCII art.
func printQRCodeToTerminal(code string) error {
	qr, err := qrcode.New(code, qrcode.Medium)
	if err != nil {
		return fmt.Errorf("failed to create QR code: %w", err)
	}

	// Get the QR code image as a 2D boolean array
	// True for black, false for white
	qrMatrix := qr.Bitmap()

	// Define characters for black and white blocks
	blackBlock := "██" // Two block characters for better aspect ratio in terminals
	whiteBlock := "  " // Two spaces for white blocks

	// Print top border
	fmt.Print("╔")
	for i := 0; i < len(qrMatrix[0])*2; i++ {
		fmt.Print("═")
	}
	fmt.Println("╗")

	// Iterate over each row and column to print the QR code
	for _, row := range qrMatrix {
		fmt.Print("║")
		for _, isBlack := range row {
			if isBlack {
				fmt.Print(blackBlock)
			} else {
				fmt.Print(whiteBlock)
			}
		}
		fmt.Println("║")
	}

	// Print bottom border
	fmt.Print("╚")
	for i := 0; i < len(qrMatrix[0])*2; i++ {
		fmt.Print("═")
	}
	fmt.Println("╝")

	return nil
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
			log.Debug("Ignoring group message", zap.String("from", v.Info.Sender.User))
			return
		}

		var text string
		if v.Message.GetConversation() != "" {
			text = v.Message.GetConversation()
		} else if v.Message.GetExtendedTextMessage() != nil {
			text = v.Message.GetExtendedTextMessage().GetText()
		}

		//log.Debug("Parsed text", zap.String("text", text))

		// Handle administrative commands
		if text != "" {
			if text == "/exclude" {
				log.Info("Executing /exclude command")
				// Display currently excluded users
				excluded := exclusionManager.GetAllExcluded()
				if len(excluded) == 0 {
					response := "No users are currently excluded from transcription."
					cli.SendMessage(context.Background(), v.Info.Chat, &waE2E.Message{
						Conversation: &response,
					})
				} else {
					response := "Currently excluded users:\n"
					for _, number := range excluded {
						response += "- " + number + "\n"
					}
					cli.SendMessage(context.Background(), v.Info.Chat, &waE2E.Message{
						Conversation: &response,
					})
				}
				return
			} else if text == "/include" {
				log.Info("Executing /include command")
				// Show error for /include without number
				response := "Usage: /include <number> - Remove a number from the exclusion list."
				cli.SendMessage(context.Background(), v.Info.Chat, &waE2E.Message{
					Conversation: &response,
				})
				return
			} else if strings.HasPrefix(text, "/exclude") {
				log.Info("Executing /exclude with number command")
				numberToExclude := strings.TrimSpace(text[8:])
				exclusionManager.Add(numberToExclude)
				response := fmt.Sprintf("%s added to exclusion list.", numberToExclude)
				cli.SendMessage(context.Background(), v.Info.Chat, &waE2E.Message{
					Conversation: &response,
				})
				return
			} else if strings.HasPrefix(text, "/include") {
				log.Info("Executing /include with number command")
				numberToInclude := strings.TrimSpace(text[8:])
				if exclusionManager.IsExcluded(numberToInclude) {
					exclusionManager.Remove(numberToInclude)
					response := fmt.Sprintf("%s removed from exclusion list.", numberToInclude)
					cli.SendMessage(context.Background(), v.Info.Chat, &waE2E.Message{
						Conversation: &response,
					})
				} else {
					response := fmt.Sprintf("%s not in exclusion list.", numberToInclude)
					cli.SendMessage(context.Background(), v.Info.Chat, &waE2E.Message{
						Conversation: &response,
					})
				}
				return
			}
		}

		destinationJID := v.Info.Chat.User
		// Check if destination is excluded
		if exclusionManager.IsExcluded(destinationJID) {
			log.Debug("Ignoring message to excluded destination", zap.String("to", destinationJID))
			return
		}

		// Check for audio messages
		if v.Message.GetAudioMessage() != nil {
			log.Info("Received audio message", zap.String("from", v.Info.Sender.User), zap.String("to", v.Info.Chat.User))
			job := transcription.NewJob(cli, v, log, transcriberService, transcriptionLanguage)
			go job.HandleAudioMessage(context.Background()) // Run in a goroutine to avoid blocking event handler
		} else {
			log.Debug("Received non-audio message", zap.String("from", v.Info.Sender.User), zap.String("to", v.Info.Chat.User), zap.String("type", v.Info.Type))
		}
	}
}
