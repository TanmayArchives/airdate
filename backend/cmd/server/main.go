package main

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/dgrijalva/jwt-go"
	"github.com/gorilla/mux"
	"github.com/jmoiron/sqlx"
	"github.com/joho/godotenv"
	_ "github.com/lib/pq"
	"github.com/rs/cors"
	"golang.org/x/crypto/bcrypt"
)

var db *sqlx.DB
var jwtKey []byte

type User struct {
	ID              int         `json:"id" db:"id"`
	Username        string      `json:"username" db:"username"`
	Password        string      `json:"-" db:"password"`
	TwitchUsername  *string     `json:"twitchUsername,omitempty" db:"twitch_username"`
	DiscordUsername *string     `json:"discordUsername,omitempty" db:"discord_username"`
	InstagramHandle *string     `json:"instagramHandle,omitempty" db:"instagram_handle"`
	YoutubeChannel  *string     `json:"youtubeChannel,omitempty" db:"youtube_channel"`
	FavoriteGames   *string     `json:"favoriteGames,omitempty" db:"favorite_games"`
	ConnectedGames  StringArray `json:"connectedGames" db:"connected_games"`
	IsPrivate       bool        `json:"isPrivate" db:"is_private"`
	FollowersCount  int         `json:"followersCount"`
	FollowingCount  int         `json:"followingCount"`
	IsFollowing     bool        `json:"isFollowing"`
}

type Claims struct {
	Username string `json:"username"`
	jwt.StandardClaims
}

type contextKey string

const userClaimsKey contextKey = "claims"

type RegisterRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type StringArray []string

func (a *StringArray) Scan(value interface{}) error {
	if value == nil {
		*a = StringArray{}
		return nil
	}

	switch v := value.(type) {
	case []byte:
		if string(v) == "{}" {
			*a = StringArray{}
			return nil
		}

		trimmed := string(v)[1 : len(string(v))-1]
		if len(trimmed) > 0 {
			*a = strings.Split(trimmed, ",")
		} else {
			*a = StringArray{}
		}
		return nil
	default:
		return fmt.Errorf("unsupported Scan, storing driver.Value type %T into type *StringArray", value)
	}
}

func (a StringArray) Value() (driver.Value, error) {
	if len(a) == 0 {
		return "{}", nil
	}
	return fmt.Sprintf("{%s}", strings.Join(a, ",")), nil
}

type UserProfile struct {
	User
	FollowersCount int  `json:"followersCount"`
	FollowingCount int  `json:"followingCount"`
	IsFollowing    bool `json:"isFollowing"`
}

func validateToken(tokenString string) (*Claims, error) {
	claims := &Claims{}
	
	token, err := jwt.ParseWithClaims(tokenString, claims, func(token *jwt.Token) (interface{}, error) {
		if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", token.Header["alg"])
		}
		return jwtKey, nil
	})

	if err != nil {
		return nil, err
	}

	if !token.Valid {
		return nil, fmt.Errorf("invalid token")
	}

	return claims, nil
}

func initDatabase() error {
	_, err := db.Exec(`
	CREATE TABLE IF NOT EXISTS users (
		id SERIAL PRIMARY KEY,
		username VARCHAR(255) UNIQUE NOT NULL,
		password VARCHAR(255) NOT NULL,
		twitch_username VARCHAR(255),
		discord_username VARCHAR(255),
		instagram_handle VARCHAR(255),
		youtube_channel VARCHAR(255),
		favorite_games TEXT[],
		connected_games TEXT[],
		is_private BOOLEAN DEFAULT false
	);
	`)
	return err
}

func main() {
	if err := godotenv.Load(); err != nil {
		log.Printf("No .env file found, using environment variables")
	}

	jwtKey = []byte(os.Getenv("JWT_SECRET"))

	dbURL := fmt.Sprintf("host=%s port=%s user=%s password=%s sslmode=disable",
		os.Getenv("DB_HOST"),
		os.Getenv("DB_PORT"),
		os.Getenv("DB_USER"),
		os.Getenv("DB_PASSWORD"),
	)

	var err error
	db, err = sqlx.Connect("postgres", dbURL)
	if err != nil {
		log.Fatalf("Error connecting to database: %v", err)
	}
	_, err = db.Exec(fmt.Sprintf("CREATE DATABASE %s", os.Getenv("DB_NAME")))
	if err != nil {
		if !strings.Contains(err.Error(), "already exists") {
			log.Fatalf("Error creating database: %v", err)
		}
	}

	db.Close()

	dbURL = fmt.Sprintf("%s dbname=%s", dbURL, os.Getenv("DB_NAME"))
	db, err = sqlx.Connect("postgres", dbURL)
	if err != nil {
		log.Fatalf("Error connecting to database: %v", err)
	}
	defer db.Close()

	// Initialize database tables
	if err := initDatabase(); err != nil {
		log.Fatalf("Error initializing database: %v", err)
	}

	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS followers (
				follower_id INTEGER REFERENCES users(id),
				following_id INTEGER REFERENCES users(id),
				created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
				PRIMARY KEY (follower_id, following_id)
		)
	`)
	if err != nil {
		log.Fatal("Error creating followers table:", err)
	}

	router := mux.NewRouter()

	// Update CORS configuration
	c := cors.New(cors.Options{
		AllowedOrigins:   []string{"http://localhost:3000"},
		AllowedMethods:   []string{"GET", "POST", "PUT", "DELETE", "OPTIONS"},
		AllowedHeaders:   []string{"Content-Type", "Accept", "Authorization", "Origin"},
		ExposedHeaders:   []string{"Authorization"},
		AllowCredentials: true,
		Debug:           true,
	})

	// Add routes
	router.HandleFunc("/register", registerHandler).Methods("POST", "OPTIONS")
	router.HandleFunc("/login", loginHandler).Methods("POST", "OPTIONS")
	router.HandleFunc("/users", getAllUsersHandler).Methods("GET")
	router.HandleFunc("/profile/{username}", getUserProfileHandler).Methods("GET")
	router.HandleFunc("/games/search", searchGamesHandler).Methods("GET", "OPTIONS")
	router.HandleFunc("/follow/{username}", authMiddleware(followUserHandler)).Methods("POST", "OPTIONS")
	router.HandleFunc("/unfollow/{username}", authMiddleware(unfollowUserHandler)).Methods("POST", "OPTIONS")
	router.HandleFunc("/profile", authMiddleware(getProfileHandler)).Methods("GET")
	router.HandleFunc("/privacy", authMiddleware(updatePrivacyHandler)).Methods("POST")
	router.HandleFunc("/connect/twitch", authMiddleware(connectTwitchHandler)).Methods("POST")
	router.HandleFunc("/connect/discord", authMiddleware(connectDiscordHandler)).Methods("POST")
	router.HandleFunc("/connect/instagram", authMiddleware(connectInstagramHandler)).Methods("POST")
	router.HandleFunc("/connect/youtube", authMiddleware(connectYoutubeHandler)).Methods("POST")
	router.HandleFunc("/connect/game", authMiddleware(connectGameHandler)).Methods("POST")
	router.HandleFunc("/disconnect/instagram", authMiddleware(disconnectInstagramHandler)).Methods("POST")
	router.HandleFunc("/disconnect/youtube", authMiddleware(disconnectYoutubeHandler)).Methods("POST")
	router.HandleFunc("/disconnect/game", authMiddleware(disconnectGameHandler)).Methods("POST")

	// Wrap router with CORS handler
	handler := c.Handler(router)

	// Start server
	log.Printf("Server starting on port 8080")
	if err := http.ListenAndServe(":8080", handler); err != nil {
		log.Fatal(err)
	}
}

func registerHandler(w http.ResponseWriter, r *http.Request) {
	log.Printf("=== Register Handler Start ===")

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "http://localhost:3000")
	w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")

	if r.Method == "OPTIONS" {
		w.WriteHeader(http.StatusOK)
		return
	}

	if err := db.Ping(); err != nil {
		log.Printf("Database connection error: %v", err)
		http.Error(w, `{"error":"Database connection error"}`, http.StatusInternalServerError)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		log.Printf("Error reading body: %v", err)
		http.Error(w, `{"error":"Error reading request body"}`, http.StatusBadRequest)
		return
	}

	log.Printf("Raw request body: %s", string(body))

	var req RegisterRequest
	if err := json.Unmarshal(body, &req); err != nil {
		log.Printf("JSON parse error: %v", err)
		http.Error(w, `{"error":"Invalid JSON format"}`, http.StatusBadRequest)
		return
	}

	username := strings.TrimSpace(req.Username)
	password := strings.TrimSpace(req.Password)

	if username == "" || password == "" {
		http.Error(w, `{"error":"Username and password are required"}`, http.StatusBadRequest)
		return
	}

	var exists bool
	err = db.Get(&exists, "SELECT EXISTS(SELECT 1 FROM users WHERE username = $1)", username)
	if err != nil {
		log.Printf("Error checking username existence: %v", err)
		http.Error(w, `{"error":"Internal server error"}`, http.StatusInternalServerError)
		return
	}
	if exists {
		http.Error(w, `{"error":"Username already exists"}`, http.StatusConflict)
		return
	}

	hashedPassword, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		log.Printf("Password hashing error: %v", err)
		http.Error(w, `{"error":"Internal server error"}`, http.StatusInternalServerError)
		return
	}

	result, err := db.Exec(`
		INSERT INTO users (username, password, connected_games) 
		VALUES ($1, $2, '{}'::text[])`,
		username, string(hashedPassword))
	if err != nil {
		log.Printf("Database error: %v", err)
		http.Error(w, `{"error":"Internal server error"}`, http.StatusInternalServerError)
		return
	}

	rows, _ := result.RowsAffected()
	log.Printf("User created successfully. Rows affected: %d", rows)

	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]string{
		"message":  "User created successfully",
		"username": username,
	})

	log.Printf("=== Register Handler End ===")
}

func loginHandler(w http.ResponseWriter, r *http.Request) {
	// Set CORS headers
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "http://localhost:3000")
	w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, Accept, Origin")
	w.Header().Set("Access-Control-Allow-Credentials", "true")

	if r.Method == "OPTIONS" {
		w.WriteHeader(http.StatusOK)
		return
	}

	var loginReq struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}

	if err := json.NewDecoder(r.Body).Decode(&loginReq); err != nil {
		http.Error(w, `{"error":"Invalid request body"}`, http.StatusBadRequest)
		return
	}

	var user User
	err := db.Get(&user, `
		SELECT id, username, password
		FROM users 
		WHERE username = $1`,
		loginReq.Username)

	if err != nil {
		if err == sql.ErrNoRows {
			http.Error(w, `{"error":"Invalid credentials"}`, http.StatusUnauthorized)
			return
		}
		log.Printf("Database error: %v", err)
		http.Error(w, `{"error":"Internal server error"}`, http.StatusInternalServerError)
		return
	}

	err = bcrypt.CompareHashAndPassword([]byte(user.Password), []byte(loginReq.Password))
	if err != nil {
		http.Error(w, `{"error":"Invalid credentials"}`, http.StatusUnauthorized)
		return
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"username": user.Username,
		"exp":      time.Now().Add(24 * time.Hour).Unix(),
	})

	tokenString, err := token.SignedString(jwtKey)
	if err != nil {
		log.Printf("Error generating token: %v", err)
		http.Error(w, `{"error":"Internal server error"}`, http.StatusInternalServerError)
		return
	}

	response := map[string]string{
		"token":    tokenString,
		"username": user.Username,
		"message":  "Login successful",
	}

	json.NewEncoder(w).Encode(response)
}

func authMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Enable CORS headers
		w.Header().Set("Access-Control-Allow-Origin", "http://localhost:3000")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		w.Header().Set("Access-Control-Allow-Credentials", "true")

		// Handle preflight requests
		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusOK)
			return
		}

		// Get token from Authorization header
		authHeader := r.Header.Get("Authorization")
		if authHeader == "" {
			http.Error(w, "Authentication required", http.StatusUnauthorized)
			return
		}

		// Extract token
		tokenString := strings.Replace(authHeader, "Bearer ", "", 1)
		claims, err := validateToken(tokenString)
		if err != nil {
			http.Error(w, "Invalid token", http.StatusUnauthorized)
			return
		}

		// Add claims to context
		ctx := context.WithValue(r.Context(), userClaimsKey, claims)
		next.ServeHTTP(w, r.WithContext(ctx))
	}
}

func profileHandler(w http.ResponseWriter, r *http.Request) {
	claims := r.Context().Value(userClaimsKey).(*Claims)
	log.Printf("=== Profile Handler Start ===")
	log.Printf("Fetching profile for user: %s", claims.Username)

	var user User
	err := db.Get(&user, `
		SELECT id, username, twitch_username, discord_username, 
			   instagram_handle, youtube_channel, favorite_games, 
			   connected_games, is_private
		FROM users WHERE username = $1`, claims.Username)

	if err != nil {
		http.Error(w, "User not found", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(user)
}

func connectTwitchHandler(w http.ResponseWriter, r *http.Request) {
	claims := r.Context().Value(userClaimsKey).(*Claims)
	var requestBody struct {
		TwitchUsername string `json:"twitchUsername"`
	}
	err := json.NewDecoder(r.Body).Decode(&requestBody)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	_, err = db.Exec("UPDATE users SET twitch_username = $1 WHERE username = $2", requestBody.TwitchUsername, claims.Username)
	if err != nil {
		log.Printf("Error connecting Twitch account: %v", err)
		http.Error(w, "Error connecting Twitch account", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"message": "Twitch account connected successfully"})
}

func connectDiscordHandler(w http.ResponseWriter, r *http.Request) {
	claims := r.Context().Value(userClaimsKey).(*Claims)
	var requestBody struct {
		DiscordUsername string `json:"discordUsername"`
	}
	err := json.NewDecoder(r.Body).Decode(&requestBody)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	_, err = db.Exec("UPDATE users SET discord_username = $1 WHERE username = $2", requestBody.DiscordUsername, claims.Username)
	if err != nil {
		log.Printf("Error connecting Discord account: %v", err)
		http.Error(w, "Error connecting Discord account", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"message": "Discord account connected successfully"})
}

func connectInstagramHandler(w http.ResponseWriter, r *http.Request) {
	claims := r.Context().Value(userClaimsKey).(*Claims)
	var requestBody struct {
		InstagramHandle string `json:"instagramHandle"`
	}
	err := json.NewDecoder(r.Body).Decode(&requestBody)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	_, err = db.Exec("UPDATE users SET instagram_handle = $1 WHERE username = $2",
		requestBody.InstagramHandle, claims.Username)
	if err != nil {
		http.Error(w, "Error connecting Instagram account", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"message": "Instagram account connected successfully"})
}

func connectYoutubeHandler(w http.ResponseWriter, r *http.Request) {
	claims := r.Context().Value(userClaimsKey).(*Claims)
	var requestBody struct {
		YoutubeChannel string `json:"youtubeChannel"`
	}
	err := json.NewDecoder(r.Body).Decode(&requestBody)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	_, err = db.Exec("UPDATE users SET youtube_channel = $1 WHERE username = $2",
		requestBody.YoutubeChannel, claims.Username)
	if err != nil {
		http.Error(w, "Error connecting YouTube account", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"message": "YouTube channel connected successfully"})
}

func searchGamesHandler(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query().Get("q")
	if query == "" {
		http.Error(w, "Search query is required", http.StatusBadRequest)
		return
	}

	query = strings.ToLower(query)

	var allGames []string
	rows, err := db.Query(`
		SELECT DISTINCT unnest(connected_games) as game
		FROM users
		WHERE LOWER(unnest(connected_games)) LIKE $1
		LIMIT 10
	`, "%"+query+"%")
	
	if err != nil {
		log.Printf("Error searching games: %v", err)
		http.Error(w, "Failed to search games", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	for rows.Next() {
		var game string
		if err := rows.Scan(&game); err != nil {
			log.Printf("Error scanning game result: %v", err)
			continue
		}
		allGames = append(allGames, game)
	}

	if len(allGames) == 0 {
		json.NewEncoder(w).Encode([]string{})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(allGames)
}

func disconnectTwitchHandler(w http.ResponseWriter, r *http.Request) {
	claims := r.Context().Value(userClaimsKey).(*Claims)

	_, err := db.Exec("UPDATE users SET twitch_username = NULL WHERE username = $1", claims.Username)
	if err != nil {
		log.Printf("Error disconnecting Twitch account: %v", err)
		http.Error(w, "Error disconnecting Twitch account", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"message": "Twitch account disconnected successfully"})
}

func disconnectDiscordHandler(w http.ResponseWriter, r *http.Request) {
	claims := r.Context().Value(userClaimsKey).(*Claims)

	_, err := db.Exec("UPDATE users SET discord_username = NULL WHERE username = $1", claims.Username)
	if err != nil {
		log.Printf("Error disconnecting Discord account: %v", err)
		http.Error(w, "Error disconnecting Discord account", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"message": "Discord account disconnected successfully"})
}

func connectGameHandler(w http.ResponseWriter, r *http.Request) {
	claims := r.Context().Value(userClaimsKey).(*Claims)

	var requestBody struct {
		GameName string `json:"gameName"`
		GameID   string `json:"gameId"`
	}

	if err := json.NewDecoder(r.Body).Decode(&requestBody); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	_, err := db.Exec(`
		UPDATE users 
		SET connected_games = array_append(connected_games, $1) 
		WHERE username = $2 AND NOT $1 = ANY(connected_games)`,
		requestBody.GameName, claims.Username)

	if err != nil {
		log.Printf("Error connecting game: %v", err)
		http.Error(w, "Error connecting game", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{
		"message": fmt.Sprintf("Successfully connected to %s", requestBody.GameName),
	})
}

func getAllUsersHandler(w http.ResponseWriter, r *http.Request) {
	var users []User
	err := db.Select(&users, `
		SELECT id, username, twitch_username, discord_username, 
			   instagram_handle, youtube_channel, favorite_games, 
			   connected_games, is_private 
		FROM users 
		ORDER BY id DESC
	`)
	
	if err != nil {
		log.Printf("Error fetching users: %v", err)
		http.Error(w, "Failed to fetch users", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(users)
}

func healthCheckHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"status": "healthy"})
}

func getUserProfileHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "http://localhost:3000")
	w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
	w.Header().Set("Access-Control-Allow-Credentials", "true")

	if r.Method == "OPTIONS" {
		w.WriteHeader(http.StatusOK)
		return
	}

	vars := mux.Vars(r)
	username := vars["username"]
	if username == "" {
		http.Error(w, "Username is required", http.StatusBadRequest)
		return
	}

	var currentUsername string
	authHeader := r.Header.Get("Authorization")
	if authHeader != "" {
		tokenString := strings.Replace(authHeader, "Bearer ", "", 1)
		claims, err := validateToken(tokenString)
		if err == nil {
			currentUsername = claims.Username
		}
	}

	var user User
	err := db.Get(&user, `
		SELECT id, username, twitch_username, discord_username, 
			   instagram_handle, youtube_channel, connected_games, 
			   is_private
		FROM users 
		WHERE username = $1`, username)

	if err != nil {
		if err == sql.ErrNoRows {
			http.Error(w, "User not found", http.StatusNotFound)
		} else {
			log.Printf("Database error: %v", err)
			http.Error(w, "Internal server error", http.StatusInternalServerError)
		}
		return
	}

	var followersCount, followingCount int
	err = db.QueryRow(`
		SELECT 
			(SELECT COUNT(*) FROM followers WHERE following_id = u.id) as followers_count,
			(SELECT COUNT(*) FROM followers WHERE follower_id = u.id) as following_count
		FROM users u 
		WHERE u.username = $1
	`, username).Scan(&followersCount, &followingCount)

	if err != nil {
		log.Printf("Error getting follower counts: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	var isFollowing bool
	if currentUsername != "" {
		err = db.QueryRow(`
			SELECT EXISTS(
				SELECT 1 FROM followers 
				WHERE follower_id = (SELECT id FROM users WHERE username = $1)
				AND following_id = (SELECT id FROM users WHERE username = $2)
			)
		`, currentUsername, username).Scan(&isFollowing)

		if err != nil {
			log.Printf("Error checking follow status: %v", err)
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}
	}

	response := struct {
		User
		FollowersCount int  `json:"followersCount"`
		FollowingCount int  `json:"followingCount"`
		IsFollowing    bool `json:"isFollowing"`
	}{
		User:           user,
		FollowersCount: followersCount,
		FollowingCount: followingCount,
		IsFollowing:    isFollowing,
	}

	if user.IsPrivate && currentUsername != username && !isFollowing {
		limitedResponse := struct {
			Username       string `json:"username"`
			IsPrivate     bool   `json:"isPrivate"`
			FollowersCount int   `json:"followersCount"`
			FollowingCount int   `json:"followingCount"`
			IsFollowing    bool  `json:"isFollowing"`
		}{
			Username:       user.Username,
			IsPrivate:     user.IsPrivate,
			FollowersCount: followersCount,
			FollowingCount: followingCount,
			IsFollowing:    isFollowing,
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(limitedResponse)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(response); err != nil {
		log.Printf("Error encoding response: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
}

func updatePrivacyHandler(w http.ResponseWriter, r *http.Request) {
	claims := r.Context().Value(userClaimsKey).(*Claims)
	if claims == nil {
		http.Error(w, `{"error": "Unauthorized"}`, http.StatusUnauthorized)
		return
	}

	var requestBody struct {
		IsPrivate bool `json:"isPrivate"`
	}

	if err := json.NewDecoder(r.Body).Decode(&requestBody); err != nil {
		log.Printf("Error decoding request body: %v", err)
		http.Error(w, `{"error": "Invalid request body"}`, http.StatusBadRequest)
		return
	}

	log.Printf("Updating privacy settings for user %s to %v", claims.Username, requestBody.IsPrivate)

	result, err := db.Exec(
		"UPDATE users SET is_private = $1 WHERE username = $2",
		requestBody.IsPrivate, claims.Username,
	)

	if err != nil {
		log.Printf("Error updating privacy settings: %v", err)
		http.Error(w, `{"error": "Error updating privacy settings"}`, http.StatusInternalServerError)
		return
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		log.Printf("Error getting rows affected: %v", err)
		http.Error(w, `{"error": "Error updating privacy settings"}`, http.StatusInternalServerError)
		return
	}

	if rowsAffected == 0 {
		http.Error(w, `{"error": "User not found"}`, http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")

	response := map[string]interface{}{
		"message":   "Privacy settings updated",
		"isPrivate": requestBody.IsPrivate,
	}

	if err := json.NewEncoder(w).Encode(response); err != nil {
		log.Printf("Error encoding response: %v", err)
		http.Error(w, `{"error": "Error encoding response"}`, http.StatusInternalServerError)
		return
	}
}

func disconnectInstagramHandler(w http.ResponseWriter, r *http.Request) {
	claims := r.Context().Value(userClaimsKey).(*Claims)

	_, err := db.Exec("UPDATE users SET instagram_handle = NULL WHERE username = $1", claims.Username)
	if err != nil {
		log.Printf("Error disconnecting Instagram account: %v", err)
		http.Error(w, "Error disconnecting Instagram account", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"message": "Instagram account disconnected successfully"})
}

func disconnectYoutubeHandler(w http.ResponseWriter, r *http.Request) {
	claims := r.Context().Value(userClaimsKey).(*Claims)

	_, err := db.Exec("UPDATE users SET youtube_channel = NULL WHERE username = $1", claims.Username)
	if err != nil {
		log.Printf("Error disconnecting YouTube account: %v", err)
		http.Error(w, "Error disconnecting YouTube account", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"message": "YouTube channel disconnected successfully"})
}

func disconnectGameHandler(w http.ResponseWriter, r *http.Request) {
	claims := r.Context().Value(userClaimsKey).(*Claims)

	var requestBody struct {
		GameName string `json:"gameName"`
	}

	if err := json.NewDecoder(r.Body).Decode(&requestBody); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	var user User
	err := db.Get(&user, "SELECT connected_games FROM users WHERE username = $1", claims.Username)
	if err != nil {
		http.Error(w, "Failed to get user games", http.StatusInternalServerError)
		return
	}

	var updatedGames StringArray
	for _, game := range user.ConnectedGames {
		if game != requestBody.GameName {
			updatedGames = append(updatedGames, game)
		}
	}

	_, err = db.Exec("UPDATE users SET connected_games = $1 WHERE username = $2",
		updatedGames, claims.Username)
	if err != nil {
		http.Error(w, "Failed to disconnect game", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{
		"message": "Game disconnected successfully",
	})
}

func followUserHandler(w http.ResponseWriter, r *http.Request) {
	claims := r.Context().Value(userClaimsKey).(*Claims)
	vars := mux.Vars(r)
	targetUsername := vars["username"]

	var followerID int
	err := db.QueryRow("SELECT id FROM users WHERE username = $1", claims.Username).Scan(&followerID)
	if err != nil {
		log.Printf("Error getting follower ID: %v", err)
		http.Error(w, "Failed to get follower ID", http.StatusInternalServerError)
		return
	}

	// Get target user ID
	var followingID int
	err = db.QueryRow("SELECT id FROM users WHERE username = $1", targetUsername).Scan(&followingID)
	if err != nil {
		if err == sql.ErrNoRows {
			http.Error(w, "User not found", http.StatusNotFound)
		} else {
			log.Printf("Error getting following ID: %v", err)
			http.Error(w, "Failed to get following ID", http.StatusInternalServerError)
		}
		return
	}

	var exists bool
	err = db.QueryRow(`
		SELECT EXISTS(
			SELECT 1 FROM followers 
			WHERE follower_id = $1 AND following_id = $2
		)
	`, followerID, followingID).Scan(&exists)

	if err != nil {
		log.Printf("Error checking existing follow: %v", err)
		http.Error(w, "Error checking follow status", http.StatusInternalServerError)
		return
	}

	if exists {
		http.Error(w, "Already following this user", http.StatusBadRequest)
		return
	}

	_, err = db.Exec(`
		INSERT INTO followers (follower_id, following_id) 
		VALUES ($1, $2)
	`, followerID, followingID)

	if err != nil {
		log.Printf("Error creating follow relationship: %v", err)
		http.Error(w, "Error following user", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"message": "Successfully followed user"})
}

func unfollowUserHandler(w http.ResponseWriter, r *http.Request) {
	claims := r.Context().Value(userClaimsKey).(*Claims)
	vars := mux.Vars(r)
	targetUsername := vars["username"]

	var followerID int
	err := db.QueryRow("SELECT id FROM users WHERE username = $1", claims.Username).Scan(&followerID)
	if err != nil {
		log.Printf("Error getting follower ID: %v", err)
		http.Error(w, "Failed to get follower ID", http.StatusInternalServerError)
		return
	}

	var followingID int
	err = db.QueryRow("SELECT id FROM users WHERE username = $1", targetUsername).Scan(&followingID)
	if err != nil {
		if err == sql.ErrNoRows {
			http.Error(w, "User not found", http.StatusNotFound)
		} else {
			log.Printf("Error getting following ID: %v", err)
			http.Error(w, "Failed to get following ID", http.StatusInternalServerError)
		}
		return
	}

	result, err := db.Exec(`
		DELETE FROM followers 
		WHERE follower_id = $1 AND following_id = $2
	`, followerID, followingID)

	if err != nil {
		log.Printf("Error unfollowing user: %v", err)
		http.Error(w, "Error unfollowing user", http.StatusInternalServerError)
		return
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		log.Printf("Error getting rows affected: %v", err)
		http.Error(w, "Error checking unfollow status", http.StatusInternalServerError)
		return
	}

	if rowsAffected == 0 {
		http.Error(w, "Not following this user", http.StatusBadRequest)
		return
	}

	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"message": "Successfully unfollowed user"})
}

func getProfileHandler(w http.ResponseWriter, r *http.Request) {
	claims := r.Context().Value(userClaimsKey).(*Claims)
	username := claims.Username

	var user User
	err := db.Get(&user, `
		SELECT * FROM users WHERE username = $1
	`, username)
	if err != nil {
		if err == sql.ErrNoRows {
			http.Error(w, "User not found", http.StatusNotFound)
			return
		}
		log.Printf("Database error: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	// Get follower and following counts
	var followersCount, followingCount int
	err = db.QueryRow(`
		SELECT 
			(SELECT COUNT(*) FROM followers WHERE following_id = u.id) as followers_count,
			(SELECT COUNT(*) FROM followers WHERE follower_id = u.id) as following_count
		FROM users u
		WHERE u.username = $1
	`, username).Scan(&followersCount, &followingCount)
	if err != nil {
		log.Printf("Error getting follow counts: %v", err)
	}

	response := struct {
		User
		FollowersCount int `json:"followersCount"`
		FollowingCount int `json:"followingCount"`
	}{
		User:           user,
		FollowersCount: followersCount,
		FollowingCount: followingCount,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}
