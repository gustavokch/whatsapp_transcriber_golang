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
	flag.StringVar(&backendFlag, "backend", "", "Transcription backend to use (groq|cloudflare|deepgram)")
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
		core = zapcore.NewCore(consoleEncoder, zapcore.AddSync(os.Stdout), zapcore.InfoLevel)
	}

	log = zap.New(core, zap.AddCaller())
	defer log.Sync()

	log.Info("Starting WhatsApp Audio Transcription Bot...")

	// Ensure data directory exists
	if err := os.MkdirAll("data", 0755); err != nil {
		log.Fatal("Failed to create data directory", zap.Error(err))
	}

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
	deepgramAPIKey := os.Getenv("DEEPGRAM_API_KEY")
	transcriptionLanguage = os.Getenv("TRANSCRIPTION_LANGUAGE")
	if transcriptionLanguage == "" {
		transcriptionLanguage = "pt" // Default to Portuguese
	}

	// Validate backend flag if provided
	if backendFlag != "" && backendFlag != "groq" && backendFlag != "cloudflare" && backendFlag != "deepgram" {
		log.Fatal("Invalid backend specified. Valid options are: groq, cloudflare, deepgram")
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
	} else if backendFlag == "deepgram" || (backendFlag == "" && deepgramAPIKey != "") {
		if deepgramAPIKey == "" {
			log.Fatal("Deepgram API key not found in environment variables")
		}
		transcriberService = transcription.NewDeepgramAITranscriber(deepgramAPIKey, log)
		backendSource := "command line flag"
		if backendFlag == "" {
			backendSource = "environment variable"
		}
		log.Info("Using Deepgram AI for transcription.", zap.String("source", backendSource))
	} else {
		log.Fatal("No transcription backend configured. Please set --backend flag or environment variables (GROQ_API_KEY, CF_ACCOUNT_ID+CF_API_KEY, or DEEPGRAM_API_KEY)")
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
func createTranscriber(backendName string) (transcription.Transcriber, error) {
	groqAPIKey := os.Getenv("GROQ_API_KEY")
	cloudflareAccountID := os.Getenv("CF_ACCOUNT_ID")
	cloudflareAPIKey := os.Getenv("CF_API_KEY")
	cloudflareModel := os.Getenv("CF_MODEL")
	deepgramAPIKey := os.Getenv("DEEPGRAM_API_KEY")

	switch backendName {
	case "groq":
		if groqAPIKey == "" {
			return nil, fmt.Errorf("groq API key not found in environment variables")
		}
		return transcription.NewGroqTranscriber(groqAPIKey, "whisper-large-v3", log), nil
	case "cloudflare":
		if cloudflareAccountID == "" || cloudflareAPIKey == "" {
			return nil, fmt.Errorf("cloudflare credentials not found in environment variables")
		}
		model := cloudflareModel
		if model == "" {
			model = "@cf/openai/whisper-large-v3-turbo"
		}
		return transcription.NewCloudflareAITranscriber(cloudflareAccountID, cloudflareAPIKey, model, log), nil
	case "deepgram":
		if deepgramAPIKey == "" {
			return nil, fmt.Errorf("deepgram API key not found in environment variables")
		}
		return transcription.NewDeepgramAITranscriber(deepgramAPIKey, log), nil
	default:
		return nil, fmt.Errorf("invalid backend specified. Valid options are: groq, cloudflare, deepgram")
	}
}

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

// extractMessageText extracts text content from different types of WhatsApp messages.
// This helper function eliminates code duplication when handling various message types.
func extractMessageText(message *waE2E.Message) string {
	if message.GetConversation() != "" {
		return message.GetConversation()
	} else if message.GetExtendedTextMessage() != nil {
		return message.GetExtendedTextMessage().GetText()
	}
	return ""
}

// handleAdminCommand processes administrative commands sent via WhatsApp.
// Returns true if a command was handled, false otherwise.
// Supported commands:
// - /exclude <number> - Add a phone number to the exclusion list
// - /exclude - Show the current exclusion list
// - /include <number> - Remove a phone number from the exclusion list
// - /include - Show usage information for the include command
// - /backend <service> - Switch transcription backend (groq|cloudflare|deepgram)
// - /backend - Show usage information for the backend command
func handleAdminCommand(v *events.Message, text string) bool {
	ctx := context.Background()

	switch {
	case text == "/exclude":
		log.Info("Executing /exclude command - displaying exclusion list")
		// Display currently excluded users
		excluded := exclusionManager.GetAllExcluded()
		var response string
		if len(excluded) == 0 {
			response = "📭 No users are currently excluded from transcription.\n\n" +
				"Use /exclude <number> to add a phone number to the exclusion list."
		} else {
			response = fmt.Sprintf("📋 Currently excluded users (%d):\n", len(excluded))
			for _, number := range excluded {
				response += "• " + number + "\n"
			}
			response += "\nUse /include <number> to remove a user from this list."
		}
		cli.SendMessage(ctx, v.Info.Chat, &waE2E.Message{
			Conversation: &response,
		})
		return true

	case text == "/include":
		log.Info("Executing /include command - showing usage")
		// Show error for /include without number
		response := "📝 Usage: /include <number>\n" +
			"Example: /include 1234567890\n\n" +
			"This command removes a phone number from the exclusion list, " +
			"allowing that user's audio messages to be transcribed again."
		cli.SendMessage(ctx, v.Info.Chat, &waE2E.Message{
			Conversation: &response,
		})
		return true

	case strings.HasPrefix(text, "/exclude "):
		log.Info("Executing /exclude with number command")
		numberToExclude := strings.TrimSpace(text[9:]) // 9 characters to skip "/exclude "
		if numberToExclude == "" {
			response := "❌ Error: Please provide a phone number to exclude.\n\n" +
				"Usage: /exclude <number>\n" +
				"Example: /exclude 1234567890"
			cli.SendMessage(ctx, v.Info.Chat, &waE2E.Message{
				Conversation: &response,
			})
			return true
		}
		exclusionManager.Add(numberToExclude)
		response := fmt.Sprintf("✅ Success! %s has been added to the exclusion list.\n\n"+
			"Audio messages from this number will no longer be transcribed.", numberToExclude)
		cli.SendMessage(ctx, v.Info.Chat, &waE2E.Message{
			Conversation: &response,
		})
		return true

	case text == "/backend":
		log.Info("Executing /backend command - showing usage")
		response := "🔧 Transcription Backend Management\n\n" +
			"Usage: /backend <service>\n" +
			"Available services: groq, cloudflare, deepgram\n\n" +
			"Example: /backend groq\n\n" +
			"This command switches the transcription service used for processing audio messages."
		cli.SendMessage(ctx, v.Info.Chat, &waE2E.Message{
			Conversation: &response,
		})
		return true

	case strings.HasPrefix(text, "/backend "):
		backendName := strings.TrimSpace(text[9:]) // 9 characters to skip "/backend "
		if backendName == "" {
			response := "❌ Error: Please specify a backend service.\n\n" +
				"Usage: /backend <service>\n" +
				"Available services: groq, cloudflare, deepgram"
			cli.SendMessage(ctx, v.Info.Chat, &waE2E.Message{
				Conversation: &response,
			})
			return true
		}

		newTranscriber, err := createTranscriber(backendName)
		if err != nil {
			response := fmt.Sprintf("❌ Error switching backend: %v\n\n"+
				"Please check your API credentials and try again.", err)
			cli.SendMessage(ctx, v.Info.Chat, &waE2E.Message{
				Conversation: &response,
			})
			return true
		}

		transcriberService = newTranscriber
		response := fmt.Sprintf("✅ Success! Switched transcription backend to %s.\n\n"+
			"New audio messages will be processed using this service.", backendName)
		cli.SendMessage(ctx, v.Info.Chat, &waE2E.Message{
			Conversation: &response,
		})
		log.Info("Transcription backend changed", zap.String("backend", backendName))
		return true

	case strings.HasPrefix(text, "/include "):
		log.Info("Executing /include with number command")
		numberToInclude := strings.TrimSpace(text[9:]) // 9 characters to skip "/include "
		if numberToInclude == "" {
			response := "❌ Error: Please provide a phone number to include.\n\n" +
				"Usage: /include <number>\n" +
				"Example: /include 1234567890"
			cli.SendMessage(ctx, v.Info.Chat, &waE2E.Message{
				Conversation: &response,
			})
			return true
		}

		if exclusionManager.IsExcluded(numberToInclude) {
			exclusionManager.Remove(numberToInclude)
			response := fmt.Sprintf("✅ Success! %s has been removed from the exclusion list.\n\n"+
				"Audio messages from this number will now be transcribed again.", numberToInclude)
			cli.SendMessage(ctx, v.Info.Chat, &waE2E.Message{
				Conversation: &response,
			})
		} else {
			response := fmt.Sprintf("ℹ️  Note: %s is not currently in the exclusion list.\n\n"+
				"No changes were made.", numberToInclude)
			cli.SendMessage(ctx, v.Info.Chat, &waE2E.Message{
				Conversation: &response,
			})
		}
		return true
	}

	return false
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

		// Extract text from different message types
		text := extractMessageText(v.Message)

		// Handle administrative commands
		if text != "" {
			handled := handleAdminCommand(v, text)
			if handled {
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
