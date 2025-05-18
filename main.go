package main

import (
    "database/sql"
    "encoding/json"
    "fmt"
    "log"
    "net/http"
    "os"
    "strings"
    "time"

    "github.com/go-telegram-bot-api/telegram-bot-api/v5"
    _ "github.com/mattn/go-sqlite3"
    "github.com/spf13/viper"
)

// Movie represents a movie or TV show
type Movie struct {
    ID        int
    Title     string
    MediaType string // "movie" or "tv"
    TMDBID    int
    UserID    int64
    WatchedAt time.Time
}

// TMDBResponse represents the TMDb API search response
type TMDBResponse struct {
    Results []struct {
        ID            int    `json:"id"`
        Title         string `json:"title"`
        Name          string `json:"name"` // For TV shows
        MediaType     string `json:"media_type"`
        ReleaseDate   string `json:"release_date"`
        FirstAirDate  string `json:"first_air_date"`
        Overview      string `json:"overview"`
    } `json:"results"`
}

var (
    bot     *tgbotapi.BotAPI
    db      *sql.DB
    tmdbKey string
)

func main() {
    // Load configuration
    viper.SetConfigName("config")
    viper.AddConfigPath(".")
    viper.SetConfigType("yaml")
    if err := viper.ReadInConfig(); err != nil {
        log.Fatalf("Error reading config: %s", err)
    }

    // Initialize bot
    var err error
    bot, err = tgbotapi.NewBotAPI(viper.GetString("telegram.token"))
    if err != nil {
        log.Fatalf("Error creating bot: %s", err)
    }
    tmdbKey = viper.GetString("tmdb.api_key")

    // Initialize database
    db, err = sql.Open("sqlite3", "./watched.db")
    if err != nil {
        log.Fatalf("Error opening database: %s", err)
    }
    defer db.Close()

    // Create table if not exists
    _, err = db.Exec(`
        CREATE TABLE IF NOT EXISTS watched (
            id INTEGER PRIMARY KEY AUTOINCREMENT,
            title TEXT,
            media_type TEXT,
            tmdb_id INTEGER,
            user_id INTEGER,
            watched_at TIMESTAMP
        )
    `)
    if err != nil {
        log.Fatalf("Error creating table: %s", err)
    }

    // Bot configuration
    bot.Debug = false
    u := tgbotapi.NewUpdate(0)
    u.Timeout = 60

    updates := bot.GetUpdatesChan(u)

    // Handle updates
    for update := range updates {
        if update.Message == nil {
            continue
        }

        chatID := update.Message.Chat.ID
        text := update.Message.Text

        switch {
        case text == "/start":
            sendMessage(chatID, "Welcome to the Movie Tracker Bot!\nCommands:\n/add - Add a watched movie or show\n/list - List watched content\n/search - Search for a movie or show")
        case strings.HasPrefix(text, "/add"):
            handleAdd(chatID, strings.TrimPrefix(text, "/add "))
        case text == "/list":
            handleList(chatID)
        case strings.HasPrefix(text, "/search"):
            handleSearch(chatID, strings.TrimPrefix(text, "/search "))
        default:
            sendMessage(chatID, "Unknown command. Use /add, /list, or /search")
        }
    }
}

func sendMessage(chatID int64, text string) {
    msg := tgbotapi.NewMessage(chatID, text)
    msg.ParseMode = "Markdown"
    if _, err := bot.Send(msg); err != nil {
        log.Printf("Error sending message: %s", err)
    }
}

func handleAdd(chatID int64, query string) {
    if query == "" {
        sendMessage(chatID, "Please provide a movie or show title: /add <title>")
        return
    }

    // Search TMDb
    results, err := searchTMDB(query)
    if err != nil || len(results.Results) == 0 {
        sendMessage(chatID, "No results found for: "+query)
        return
    }

    // Use first result
    result := results.Results[0]
    title := result.Title
    if result.MediaType == "tv" {
        title = result.Name
    }

    // Save to database
    _, err = db.Exec(
        "INSERT INTO watched (title, media_type, tmdb_id, user_id, watched_at) VALUES (?, ?, ?, ?, ?)",
        title, result.MediaType, result.ID, chatID, time.Now(),
    )
    if err != nil {
        sendMessage(chatID, "Error saving to database")
        log.Printf("Database error: %s", err)
        return
    }

    sendMessage(chatID, fmt.Sprintf("Added *%s* (%s) to your watched list!", title, result.MediaType))
}

func handleList(chatID int64) {
    rows, err := db.Query("SELECT title, media_type, watched_at FROM watched WHERE user_id = ? ORDER BY watched_at DESC", chatID)
    if err != nil {
        sendMessage(chatID, "Error retrieving list")
        log.Printf("Database error: %s", err)
        return
    }
    defer rows.Close()

    var response strings.Builder
    response.WriteString("Your watched list:\n")
    count := 0

    for rows.Next() {
        var title, mediaType string
        var watchedAt time.Time
        if err := rows.Scan(&title, &mediaType, &watchedAt); err != nil {
            log.Printf("Error scanning row: %s", err)
            continue
        }
        count++
        response.WriteString(fmt.Sprintf("%d. *%s* (%s) - Watched on %s\n", count, title, mediaType, watchedAt.Format("2006-01-02")))
    }

    if count == 0 {
        sendMessage(chatID, "Your watched list is empty")
        return
    }

    sendMessage(chatID, response.String())
}

func handleSearch(chatID int64, query string) {
    if query == "" {
        sendMessage(chatID, "Please provide a search term: /search <title>")
        return
    }

    results, err := searchTMDB(query)
    if err != nil || len(results.Results) == 0 {
        sendMessage(chatID, "No results found for: "+query)
        return
    }

    var response strings.Builder
    response.WriteString("Search results:\n")
    for i, result := range results.Results[:min(5, len(results.Results))] {
        title := result.Title
        date := result.ReleaseDate
        if result.MediaType == "tv" {
            title = result.Name
            date = result.FirstAirDate
        }
        response.WriteString(fmt.Sprintf("%d. *%s* (%s, %s) - %s\n", i+1, title, result.MediaType, date, limitString(result.Overview, 100)))
    }

    sendMessage(chatID, response.String())
}

func searchTMDB(query string) (TMDBResponse, error) {
    var response TMDBResponse
    url := fmt.Sprintf("https://api.themoviedb.org/3/search/multi?api_key=%s&query=%s", tmdbKey, url.QueryEscape(query))
    
    resp, err := http.Get(url)
    if err != nil {
        return response, err
    }
    defer resp.Body.Close()

    if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
        return response, err
    }

    return response, nil
}

func min(a, b int) int {
    if a < b {
        return a
    }
    return b
}

func limitString(s string, n int) string {
    if len(s) <= n {
        return s
    }
    return s[:n] + "..."
}
