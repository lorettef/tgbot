package main

import (
    "database/sql"
    "encoding/json"
    "fmt"
    "log"
    "net/http"
    "net/url"
    "strconv"
    "strings"
    "time"

    "github.com/go-telegram-bot-api/telegram-bot-api/v5"
    _ "github.com/mattn/go-sqlite3"
    "github.com/spf13/viper"
)

// Movie represents a movie or TV show
type Movie struct {
    ID            int
    Title         string
    MediaType     string // "movie" or "tv"
    TMDBID        int
    UserID        int64
    WatchedAt     time.Time
    CurrentEpisode int // Added for TV shows
}

// TMDBResponse represents the TMDb API search response
type TMDBResponse struct {
    Results []struct {
        ID            int     `json:"id"`
        Title         string  `json:"title"`
        Name          string  `json:"name"` // For TV shows
        MediaType     string  `json:"media_type"`
        ReleaseDate   string  `json:"release_date"`
        FirstAirDate  string  `json:"first_air_date"`
        Overview      string  `json:"overview"`
        PosterPath    string  `json:"poster_path"` // For poster
        Popularity    float64 `json:"popularity"`  // For top lists
    } `json:"results"`
}

// ConversationState tracks the state of user interactions
type ConversationState struct {
    AwaitingEpisode bool
    TMDBID          int
    Title           string
    MediaType       string
}

var (
    bot            *tgbotapi.BotAPI
    db             *sql.DB
    tmdbKey        string
    conversationStates map[int64]ConversationState // Map to track conversation state
)

func main() {
    // Initialize conversation state map
    conversationStates = make(map[int64]ConversationState)

    // Load configuration
    viper.SetConfigName("config")
    viper.AddConfigPath(".")
    viper.SetConfigType("yaml")
    if err := viper.ReadInConfig(); err != nil {
        log.Fatalf("Ошибка чтения конфигурации: %s", err)
    }

    // Initialize bot
    var err error
    bot, err = tgbotapi.NewBotAPI(viper.GetString("telegram.token"))
    if err != nil {
        log.Fatalf("Ошибка создания бота: %s", err)
    }
    tmdbKey = viper.GetString("tmdb.api_key")

    // Initialize database
    db, err = sql.Open("sqlite3", "./watched.db")
    if err != nil {
        log.Fatalf("Ошибка открытия базы данных: %s", err)
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
            watched_at TIMESTAMP,
            current_episode INTEGER DEFAULT 0
        )
    `)
    if err != nil {
        log.Fatalf("Ошибка создания таблицы: %s", err)
    }

    // Add current_episode column if it doesn't exist
    _, err = db.Exec(`ALTER TABLE watched ADD COLUMN current_episode INTEGER DEFAULT 0`)
    if err != nil && !strings.Contains(err.Error(), "duplicate column name") {
        log.Printf("Ошибка добавления столбца current_episode: %s", err)
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

        // Check if user is responding with an episode number
        if state, exists := conversationStates[chatID]; exists && state.AwaitingEpisode {
            handleEpisodeInput(chatID, text, state)
            continue
        }

        switch {
        case text == "/start":
            sendMessage(chatID, "Добро пожаловать в Movie Tracker Bot!\nКоманды:\n/add - Добавить просмотренный фильм или сериал\n/list - Показать список просмотренного\n/search - Найти фильм или сериал\n/top - Топ-20 фильмов и сериалов за неделю\n/update - Обновить номер серии для сериала")
        case strings.HasPrefix(text, "/add"):
            handleAdd(chatID, strings.TrimPrefix(text, "/add "))
        case text == "/list":
            handleList(chatID)
        case strings.HasPrefix(text, "/search"):
            handleSearch(chatID, strings.TrimPrefix(text, "/search "))
        case text == "/top":
            handleTop(chatID)
        case strings.HasPrefix(text, "/update"):
            handleUpdate(chatID, strings.TrimPrefix(text, "/update "))
        default:
            sendMessage(chatID, "Неизвестная команда. Используйте /add, /list, /search, /top или /update")
        }
    }
}

func sendMessage(chatID int64, text string) {
    msg := tgbotapi.NewMessage(chatID, text)
    msg.ParseMode = "Markdown"
    if _, err := bot.Send(msg); err != nil {
        log.Printf("Ошибка отправки сообщения: %s", err)
    }
}

func sendPhoto(chatID int64, photoURL, caption string) {
    msg := tgbotapi.NewPhoto(chatID, tgbotapi.FileURL(photoURL))
    msg.Caption = caption
    msg.ParseMode = "Markdown"
    if _, err := bot.Send(msg); err != nil {
        log.Printf("Ошибка отправки фото: %s", err)
    }
}

func handleAdd(chatID int64, query string) {
    if query == "" {
        sendMessage(chatID, "Укажите название фильма или сериала: /add <название>")
        return
    }

    // Search TMDb
    results, err := searchTMDB(query)
    if err != nil || len(results.Results) == 0 {
        sendMessage(chatID, "Ничего не найдено для: "+query)
        return
    }

    // Use first result
    result := results.Results[0]
    title := result.Title
    mediaType := "фильм"
    if result.MediaType == "tv" {
        title = result.Name
        mediaType = "сериал"
    }

    if result.MediaType == "tv" {
        // Save to conversation state and ask for episode number
        conversationStates[chatID] = ConversationState{
            AwaitingEpisode: true,
            TMDBID:         result.ID,
            Title:          title,
            MediaType:      result.MediaType,
        }
        sendMessage(chatID, fmt.Sprintf("Вы добавляете сериал *%s*. Укажите номер последней просмотренной серии (например, 5):", title))
        return
    }

    // For movies, save directly to database
    _, err = db.Exec(
        "INSERT INTO watched (title, media_type, tmdb_id, user_id, watched_at, current_episode) VALUES (?, ?, ?, ?, ?, ?)",
        title, result.MediaType, result.ID, chatID, time.Now(), 0,
    )
    if err != nil {
        sendMessage(chatID, "Ошибка сохранения в базу данных")
        log.Printf("Ошибка базы данных: %s", err)
        return
    }

    // Send confirmation with poster
    message := fmt.Sprintf("Добавлено *%s* (%s) в ваш список просмотренного!", title, mediaType)
    if result.PosterPath != "" {
        posterURL := fmt.Sprintf("https://image.tmdb.org/t/p/w500%s", result.PosterPath)
        sendPhoto(chatID, posterURL, message)
    } else {
        sendMessage(chatID, message)
    }
}

func handleEpisodeInput(chatID int64, text string, state ConversationState) {
    episode, err := strconv.Atoi(text)
    if err != nil || episode < 0 {
        sendMessage(chatID, "Пожалуйста, укажите корректный номер серии (целое число, например, 5):")
        return
    }

    // Save to database
    _, err = db.Exec(
        "INSERT INTO watched (title, media_type, tmdb_id, user_id, watched_at, current_episode) VALUES (?, ?, ?, ?, ?, ?)",
        state.Title, state.MediaType, state.TMDBID, chatID, time.Now(), episode,
    )
    if err != nil {
        sendMessage(chatID, "Ошибка сохранения в базу данных")
        log.Printf("Ошибка базы данных: %s", err)
        return
    }

    // Clear conversation state
    delete(conversationStates, chatID)

    // Send confirmation with poster
    results, err := searchTMDB(state.Title)
    if err == nil && len(results.Results) > 0 && results.Results[0].ID == state.TMDBID {
        if results.Results[0].PosterPath != "" {
            posterURL := fmt.Sprintf("https://image.tmdb.org/t/p/w500%s", results.Results[0].PosterPath)
            sendPhoto(chatID, posterURL, fmt.Sprintf("Добавлено *%s* (сериал, серия %d) в ваш список просмотренного!", state.Title, episode))
            return
        }
    }
    sendMessage(chatID, fmt.Sprintf("Добавлено *%s* (сериал, серия %d) в ваш список просмотренного!", state.Title, episode))
}

func handleList(chatID int64) {
    rows, err := db.Query("SELECT title, media_type, watched_at, current_episode FROM watched WHERE user_id = ? ORDER BY watched_at DESC", chatID)
    if err != nil {
        sendMessage(chatID, "Ошибка получения списка")
        log.Printf("Ошибка базы данных: %s", err)
        return
    }
    defer rows.Close()

    var response strings.Builder
    response.WriteString("Ваш список просмотренного:\n")
    count := 0

    for rows.Next() {
        var title, mediaType string
        var watchedAt time.Time
        var currentEpisode int
        if err := rows.Scan(&title, &mediaType, &watchedAt, &currentEpisode); err != nil {
            log.Printf("Ошибка чтения строки: %s", err)
            continue
        }
        count++
        mediaTypeStr := "фильм"
        if mediaType == "tv" {
            mediaTypeStr = "сериал"
            response.WriteString(fmt.Sprintf("%d. *%s* (%s, серия %d) - Просмотрено %s\n", count, title, mediaTypeStr, currentEpisode, watchedAt.Format("2006-01-02")))
        } else {
            response.WriteString(fmt.Sprintf("%d. *%s* (%s) - Просмотрено %s\n", count, title, mediaTypeStr, watchedAt.Format("2006-01-02")))
        }
    }

    if count == 0 {
        sendMessage(chatID, "Ваш список просмотренного пуст")
        return
    }

    sendMessage(chatID, response.String())
}

func handleSearch(chatID int64, query string) {
    if query == "" {
        sendMessage(chatID, "Укажите поисковый запрос: /search <название>")
        return
    }

    results, err := searchTMDB(query)
    if err != nil || len(results.Results) == 0 {
        sendMessage(chatID, "Ничего не найдено для: "+query)
        return
    }

    for i, result := range results.Results[:min(5, len(results.Results))] {
        title := result.Title
        date := result.ReleaseDate
        mediaType := "фильм"
        if result.MediaType == "tv" {
            title = result.Name
            date = result.FirstAirDate
            mediaType = "сериал"
        }
        message := fmt.Sprintf("%d. *%s* (%s, %s) - %s", i+1, title, mediaType, date, limitString(result.Overview, 100))
        if result.PosterPath != "" {
            posterURL := fmt.Sprintf("https://image.tmdb.org/t/p/w500%s", result.PosterPath)
            sendPhoto(chatID, posterURL, message)
        } else {
            sendMessage(chatID, message)
        }
    }
}

func handleTop(chatID int64) {
    // Fetch top movies
    movies, err := getTopMovies()
    if err != nil {
        sendMessage(chatID, "Ошибка получения топ-фильмов")
        log.Printf("Ошибка получения топ-фильмов: %s", err)
        return
    }

    // Fetch top TV shows
    shows, err := getTopTVShows()
    if err != nil {
        sendMessage(chatID, "Ошибка получения топ-сериалов")
        log.Printf("Ошибка получения топ-сериалов: %s", err)
        return
    }

    // Combine and sort by popularity
    allResults := append(movies.Results, shows.Results...)
    if len(allResults) == 0 {
        sendMessage(chatID, "Топ-фильмы и сериалы не найдены")
        return
    }

    // Sort by popularity (descending)
    sortResultsByPopularity(allResults)

    // Send top 20 results
    for i, result := range allResults[:min(20, len(allResults))] {
        title := result.Title
        date := result.ReleaseDate
        mediaType := "фильм"
        if result.MediaType == "tv" {
            title = result.Name
            date = result.FirstAirDate
            mediaType = "сериал"
        }
        message := fmt.Sprintf("%d. *%s* (%s, %s) - %s", i+1, title, mediaType, date, limitString(result.Overview, 100))
        if result.PosterPath != "" {
            posterURL := fmt.Sprintf("https://image.tmdb.org/t/p/w500%s", result.PosterPath)
            sendPhoto(chatID, posterURL, message)
        } else {
            sendMessage(chatID, message)
        }
    }
}

func handleUpdate(chatID int64, query string) {
    if query == "" {
        sendMessage(chatID, "Укажите название сериала и номер серии: /update <название> <номер серии>")
        return
    }

    parts := strings.Fields(query)
    if len(parts) < 2 {
        sendMessage(chatID, "Укажите название сериала и номер серии: /update <название> <номер серии>")
        return
    }

    episode, err := strconv.Atoi(parts[len(parts)-1])
    if err != nil || episode < 0 {
        sendMessage(chatID, "Укажите корректный номер серии (целое число, например, 5)")
        return
    }

    title := strings.Join(parts[:len(parts)-1], " ")
    // Check if the title exists in the user's watched list and is a TV show
    var tmdbID int
    var mediaType string
    err = db.QueryRow("SELECT tmdb_id, media_type FROM watched WHERE user_id = ? AND title = ?", chatID, title).Scan(&tmdbID, &mediaType)
    if err != nil {
        sendMessage(chatID, "Сериал не найден в вашем списке просмотренного")
        return
    }
    if mediaType != "tv" {
        sendMessage(chatID, "Это не сериал. Используйте /update только для сериалов")
        return
    }

    // Update episode number
    _, err = db.Exec("UPDATE watched SET current_episode = ? WHERE user_id = ? AND tmdb_id = ?", episode, chatID, tmdbID)
    if err != nil {
        sendMessage(chatID, "Ошибка обновления номера серии")
        log.Printf("Ошибка базы данных: %s", err)
        return
    }

    sendMessage(chatID, fmt.Sprintf("Обновлено: *%s* (сериал, серия %d)", title, episode))
}

func getTopMovies() (TMDBResponse, error) {
    var response TMDBResponse
    urlStr := fmt.Sprintf("https://api.themoviedb.org/3/movie/popular?api_key=%s&language=ru-RU", tmdbKey)
    
    resp, err := http.Get(urlStr)
    if err != nil {
        return response, err
    }
    defer resp.Body.Close()

    if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
        return response, err
    }

    // Set media_type for movies
    for i := range response.Results {
        response.Results[i].MediaType = "movie"
    }

    return response, nil
}

func getTopTVShows() (TMDBResponse, error) {
    var response TMDBResponse
    urlStr := fmt.Sprintf("https://api.themoviedb.org/3/tv/popular?api_key=%s&language=ru-RU", tmdbKey)
    
    resp, err := http.Get(urlStr)
    if err != nil {
        return response, err
    }
    defer resp.Body.Close()

    if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
        return response, err
    }

    // Set media_type for TV shows
    for i := range response.Results {
        response.Results[i].MediaType = "tv"
    }

    return response, nil
}

func sortResultsByPopularity(results []struct {
    ID            int     `json:"id"`
    Title         string  `json:"title"`
    Name          string  `json:"name"`
    MediaType     string  `json:"media_type"`
    ReleaseDate   string  `json:"release_date"`
    FirstAirDate  string  `json:"first_air_date"`
    Overview      string  `json:"overview"`
    PosterPath    string  `json:"poster_path"`
    Popularity    float64 `json:"popularity"`
}) {
    // Simple bubble sort for simplicity
    for i := 0; i < len(results)-1; i++ {
        for j := 0; j < len(results)-i-1; j++ {
            if results[j].Popularity < results[j+1].Popularity {
                results[j], results[j+1] = results[j+1], results[j]
            }
        }
    }
}

func searchTMDB(query string) (TMDBResponse, error) {
    var response TMDBResponse
    urlStr := fmt.Sprintf("https://api.themoviedb.org/3/search/multi?api_key=%s&query=%s&language=ru-RU", tmdbKey, url.QueryEscape(query))
    
    resp, err := http.Get(urlStr)
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
