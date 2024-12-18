package main

import (
	"bytes"
	"crypto/ecdsa"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dgrijalva/jwt-go"
	"github.com/felixge/fgprof"
	"github.com/go-sql-driver/mysql"
	"github.com/goccy/go-json"
	"github.com/gorilla/sessions"
	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo-contrib/session"
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"github.com/labstack/gommon/log"
	"golang.org/x/exp/maps"
)

const (
	sessionName                 = "isucondition_go"
	conditionLimit              = 20
	frontendContentsPath        = "../public"
	jiaJWTSigningKeyPath        = "../ec256-public.pem"
	defaultIconFilePath         = "../NoImage.jpg"
	defaultJIAServiceURL        = "http://localhost:5000"
	mysqlErrNumDuplicateEntry   = 1062
	conditionLevelInfo          = "info"
	conditionLevelWarning       = "warning"
	conditionLevelCritical      = "critical"
	scoreConditionLevelInfo     = 3
	scoreConditionLevelWarning  = 2
	scoreConditionLevelCritical = 1
)

var (
	db                  *sqlx.DB
	mySQLConnectionData *MySQLConnectionEnv

	jiaJWTSigningKey *ecdsa.PublicKey

	postIsuConditionTargetBaseURL string // JIAへのactivate時に登録する，ISUがconditionを送る先のURL
	isuCache                      *IsuCache
	userCache                     *UserCache
	isuConditionCache             *IsuConditionCache
	defaultIcon                   []byte
	unixDomainSockPath            = "/tmp/isucondition.sock"
)

type Config struct {
	Name string `db:"name"`
	URL  string `db:"url"`
}

type Isu struct {
	ID         int    `db:"id"           json:"id"`
	JIAIsuUUID string `db:"jia_isu_uuid" json:"jia_isu_uuid"`
	Name       string `db:"name"         json:"name"`
	Image      []byte `db:"image"        json:"-"`
	Character  string `db:"character"    json:"character"`
	JIAUserID  string `db:"jia_user_id"  json:"-"`
}

type IsuFromJIA struct {
	Character string `json:"character"`
}

type GetIsuListResponse struct {
	ID                 int                      `json:"id"`
	JIAIsuUUID         string                   `json:"jia_isu_uuid"`
	Name               string                   `json:"name"`
	Character          string                   `json:"character"`
	LatestIsuCondition *GetIsuConditionResponse `json:"latest_isu_condition"`
}

type IsuCondition struct {
	ID         int       `db:"id"`
	JIAIsuUUID string    `db:"jia_isu_uuid"`
	Timestamp  time.Time `db:"timestamp"`
	IsSitting  bool      `db:"is_sitting"`
	Condition  string    `db:"condition"`
	Message    string    `db:"message"`
	Level      string    `db:"level"`
}

type MySQLConnectionEnv struct {
	Host     string
	Port     string
	User     string
	DBName   string
	Password string
}

type InitializeRequest struct {
	JIAServiceURL string `json:"jia_service_url"`
}

type InitializeResponse struct {
	Language string `json:"language"`
}

type GetMeResponse struct {
	JIAUserID string `json:"jia_user_id"`
}

type GraphResponse struct {
	StartAt             int64           `json:"start_at"`
	EndAt               int64           `json:"end_at"`
	Data                *GraphDataPoint `json:"data"`
	ConditionTimestamps []int64         `json:"condition_timestamps"`
}

type GraphDataPoint struct {
	Score      int                  `json:"score"`
	Percentage ConditionsPercentage `json:"percentage"`
}

type ConditionsPercentage struct {
	Sitting      int `json:"sitting"`
	IsBroken     int `json:"is_broken"`
	IsDirty      int `json:"is_dirty"`
	IsOverweight int `json:"is_overweight"`
}

type GraphDataPointWithInfo struct {
	JIAIsuUUID          string
	StartAt             time.Time
	Data                GraphDataPoint
	ConditionTimestamps []int64
}

type GetIsuConditionResponse struct {
	JIAIsuUUID     string `json:"jia_isu_uuid"`
	IsuName        string `json:"isu_name"`
	Timestamp      int64  `json:"timestamp"`
	IsSitting      bool   `json:"is_sitting"`
	Condition      string `json:"condition"`
	ConditionLevel string `json:"condition_level"`
	Message        string `json:"message"`
}

type TrendResponse struct {
	Character string            `json:"character"`
	Info      []*TrendCondition `json:"info"`
	Warning   []*TrendCondition `json:"warning"`
	Critical  []*TrendCondition `json:"critical"`
}

type TrendCondition struct {
	ID        int   `json:"isu_id"`
	Timestamp int64 `json:"timestamp"`
}

type PostIsuConditionRequest struct {
	IsSitting bool   `json:"is_sitting"`
	Condition string `json:"condition"`
	Message   string `json:"message"`
	Timestamp int64  `json:"timestamp"`
}

type JIAServiceRequest struct {
	TargetBaseURL string `json:"target_base_url"`
	IsuUUID       string `json:"isu_uuid"`
}

type IsuConditionCache struct {
	cache map[string]*IsuCondition
	Lock  sync.Mutex
}

func (cc *IsuConditionCache) Get(jiaIsuUUID string) (*IsuCondition, error) {
	cc.Lock.Lock()
	defer cc.Lock.Unlock()
	cond, ok := cc.cache[jiaIsuUUID]
	if !ok {
		var i IsuCondition
		err := db.Get(
			&i,
			"SELECT  `jia_isu_uuid`, `timestamp`, `is_sitting`, `condition`, `message`, `level` FROM `isu_condition` WHERE `jia_isu_uuid` = ? ORDER BY `timestamp` DESC LIMIT 1",
			jiaIsuUUID,
		)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil, sql.ErrNoRows
			}
			return nil, err
		}
		cc.cache[jiaIsuUUID] = &i
		return &i, nil
	}
	return cond, nil
}

func (cc *IsuConditionCache) Forget(jiaIsuUUID string) {
	cc.Lock.Lock()
	defer cc.Lock.Unlock()
	delete(cc.cache, jiaIsuUUID)
}

type IsuCache struct {
	cache map[string]*Isu
	Lock  sync.Mutex
}

func (ic *IsuCache) Get(jiaIsuUUID string) (*Isu, error) {
	ic.Lock.Lock()
	defer ic.Lock.Unlock()
	isu, ok := ic.cache[jiaIsuUUID]
	if !ok {
		var i Isu
		err := db.Get(
			&i,
			"SELECT `id`, `jia_isu_uuid`, `name`, `image`, `character`, `jia_user_id` FROM `isu` WHERE `jia_isu_uuid` = ?",
			jiaIsuUUID,
		)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil, sql.ErrNoRows
			}
			return nil, err
		}
		ic.cache[jiaIsuUUID] = &i
		return &i, nil
	}
	return isu, nil
}

func (ic *IsuCache) Forget(jiaIsuUUID string) {
	ic.Lock.Lock()
	defer ic.Lock.Unlock()
	delete(ic.cache, jiaIsuUUID)
}

type UserCache struct {
	cache map[string]struct{}
	Lock  sync.Mutex
}

func (uc *UserCache) Get(jiaUserID string) (bool, error) {
	uc.Lock.Lock()
	defer uc.Lock.Unlock()
	_, ok := uc.cache[jiaUserID]
	if !ok {
		var count int
		err := db.Get(&count, "SELECT 1 FROM `user` WHERE `jia_user_id` = ?",
			jiaUserID)
		if err != nil {
			return false, fmt.Errorf("db error: %v", err)
		}
		if count == 0 {
			return false, sql.ErrNoRows
		}
		uc.cache[jiaUserID] = struct{}{}
		return true, nil
	}
	return ok, nil
}

type TrendCache struct {
	res  []TrendResponse
	Lock sync.Mutex
}

func (tc *TrendCache) Get() []TrendResponse {
	return tc.res
}

func (tc *TrendCache) Set(res []TrendResponse) {
	tc.Lock.Lock()
	defer tc.Lock.Unlock()
	tc.res = res
}

var trendCache *TrendCache

func NewTrendCache() *TrendCache {
	return &TrendCache{
		res: make([]TrendResponse, 0, 1024),
	}
}

type InsertQueue struct {
	Queue []IsuCondition
	Lock  sync.Mutex
}

const queueSize = 10240

var insertQueue *InsertQueue

func (iq *InsertQueue) Insert(conds []IsuCondition) {
	iq.Lock.Lock()
	defer iq.Lock.Unlock()
	iq.Queue = append(iq.Queue, conds...)
}

func (iq *InsertQueue) PopAll() []IsuCondition {
	iq.Lock.Lock()
	defer iq.Lock.Unlock()
	queue := iq.Queue
	iq.Queue = make([]IsuCondition, 0, queueSize)
	return queue
}

func NewQueue() *InsertQueue {
	return &InsertQueue{
		Queue: make([]IsuCondition, 0, queueSize),
	}
}

func getEnv(key string, defaultValue string) string {
	val := os.Getenv(key)
	if val != "" {
		return val
	}
	return defaultValue
}

func NewMySQLConnectionEnv() *MySQLConnectionEnv {
	return &MySQLConnectionEnv{
		Host:     getEnv("MYSQL_HOST", "127.0.0.1"),
		Port:     getEnv("MYSQL_PORT", "3306"),
		User:     getEnv("MYSQL_USER", "isucon"),
		DBName:   getEnv("MYSQL_DBNAME", "isucondition"),
		Password: getEnv("MYSQL_PASS", "isucon"),
	}
}

func (mc *MySQLConnectionEnv) ConnectDB() (*sqlx.DB, error) {
	dsn := fmt.Sprintf(
		"%v:%v@tcp(%v:%v)/%v?parseTime=true&loc=Asia%%2FTokyo&interpolateParams=true&maxAllowedPacket=0",
		mc.User,
		mc.Password,
		mc.Host,
		mc.Port,
		mc.DBName,
	)
	return sqlx.Open("mysql", dsn)
}

type JSONSerializer struct{}

func (j *JSONSerializer) Serialize(c echo.Context, i interface{}, indent string) error {
	enc := json.NewEncoder(c.Response())
	return enc.Encode(i)
}

func (j *JSONSerializer) Deserialize(c echo.Context, i interface{}) error {
	err := json.NewDecoder(c.Request().Body).Decode(i)
	if ute, ok := err.(*json.UnmarshalTypeError); ok {
		return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("Unmarshal type error: expected=%v, got=%v, field=%v, offset=%v", ute.Type, ute.Value, ute.Field, ute.Offset)).
			SetInternal(err)
	} else if se, ok := err.(*json.SyntaxError); ok {
		return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("Syntax error: offset=%v, error=%v", se.Offset, se.Error())).SetInternal(err)
	}
	return err
}

func init() {
	key, err := os.ReadFile(jiaJWTSigningKeyPath)
	if err != nil {
		log.Fatalf("failed to read file: %v", err)
	}
	jiaJWTSigningKey, err = jwt.ParseECPublicKeyFromPEM(key)
	if err != nil {
		log.Fatalf("failed to parse ECDSA public key: %v", err)
	}

	insertQueue = NewQueue()
	trendCache = NewTrendCache()

	defaultIcon, err = os.ReadFile(defaultIconFilePath)
	if err != nil {
		log.Fatalf("failed to read file: %v", err)
	}

	isuCache = &IsuCache{
		cache: make(map[string]*Isu),
	}
	userCache = &UserCache{
		cache: make(map[string]struct{}),
	}
	isuConditionCache = &IsuConditionCache{
		cache: make(map[string]*IsuCondition),
	}

	http.DefaultTransport.(*http.Transport).MaxIdleConns = 0                // infinite
	http.DefaultTransport.(*http.Transport).MaxIdleConnsPerHost = 1024 * 16 // default: 2
	// http.DefaultTransport.(*http.Transport).ForceAttemptHTTP2 = true        // go1.13以上
}

func newUnixDomainSockListener() (net.Listener, bool, error) {
	if len(unixDomainSockPath) == 0 {
		return nil, false, nil
	}

	err := os.Remove(unixDomainSockPath)
	if err != nil && !os.IsNotExist(err) {
		return nil, false, fmt.Errorf("failed to remove socket file: %w", err)
	}

	listener, err := net.Listen("unix", unixDomainSockPath)
	if err != nil {
		return nil, false, fmt.Errorf("unix domain sock listen error: %w", err)
	}

	err = os.Chmod(unixDomainSockPath, 0777)
	if err != nil {
		listener.Close()
		return nil, false, fmt.Errorf("unix domain sock chmod error: %w", err)
	}

	return listener, true, nil
}

func main() {
	e := echo.New()
	e.JSONSerializer = &JSONSerializer{}
	// e.JSONSerializer = fj4echo.New()
	e.Use(middleware.Recover())
	e.POST("/initialize", postInitialize)

	e.Use(
		session.Middleware(sessions.NewCookieStore([]byte(getEnv("SESSION_KEY", "isucondition")))),
	)

	e.POST("/api/auth", postAuthentication)
	e.POST("/api/signout", postSignout)
	e.GET("/api/user/me", getMe)
	e.GET("/api/isu", getIsuList)
	e.POST("/api/isu", postIsu)
	e.GET("/api/isu/:jia_isu_uuid", getIsuID)
	e.GET("/api/isu/:jia_isu_uuid/icon", getIsuIcon)
	e.GET("/api/isu/:jia_isu_uuid/graph", getIsuGraph)
	e.GET("/api/condition/:jia_isu_uuid", getIsuConditions)
	e.GET("/api/trend", getTrend)

	e.POST("/api/condition/:jia_isu_uuid", postIsuCondition)

	// e.GET("/", getIndex)
	// e.GET("/isu/:jia_isu_uuid", getIndex)
	// e.GET("/isu/:jia_isu_uuid/condition", getIndex)
	// e.GET("/isu/:jia_isu_uuid/graph", getIndex)
	// e.GET("/register", getIndex)
	// e.Static("/assets", frontendContentsPath+"/assets")

	http.DefaultServeMux.Handle("/debug/fgprof", fgprof.Handler())
	go func() {
		fmt.Println(http.ListenAndServe(":6060", nil))
	}()
	mySQLConnectionData = NewMySQLConnectionEnv()

	var err error
	db, err = mySQLConnectionData.ConnectDB()
	if err != nil {
		e.Logger.Fatalf("failed to connect db: %v", err)
		return
	}
	db.SetMaxOpenConns(1024)
	db.SetMaxIdleConns(1024)
	defer db.Close()

	postIsuConditionTargetBaseURL = os.Getenv("POST_ISUCONDITION_TARGET_BASE_URL")
	if postIsuConditionTargetBaseURL == "" {
		e.Logger.Fatalf("missing: POST_ISUCONDITION_TARGET_BASE_URL")
		return
	}

	if os.Getenv("SRVNO") == "1" {
		go insertIsuConditionScheduled(time.Millisecond * 100)
		listener, isUnixDomainSock, err := newUnixDomainSockListener()
		if err != nil {
			e.Logger.Fatalf("failed to create unix domain socket listener: %v", err)
			return
		}

		if isUnixDomainSock {
			e.Listener = listener
		}
		go calculateTrendScheduled(time.Millisecond * 100)
	}

	serverPort := fmt.Sprintf(":%v", getEnv("SERVER_APP_PORT", "3000"))
	e.Logger.Fatal(e.Start(serverPort))
}

func getUserIDFromSession(c echo.Context) (string, int, error) {
	session, err := session.Get(sessionName, c)
	if err != nil {
		c.Logger().Error(err)
		return "", http.StatusInternalServerError, fmt.Errorf("failed to get session: %v", err)
	}
	_jiaUserID, ok := session.Values["jia_user_id"]
	if !ok {
		c.Logger().Errorf("no session")
		return "", http.StatusUnauthorized, fmt.Errorf("no session")
	}

	jiaUserID := _jiaUserID.(string)

	if _, err := userCache.Get(jiaUserID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			c.Logger().Errorf("not found: user")
			return "", http.StatusUnauthorized, fmt.Errorf("not found: user")
		}
		c.Logger().Errorf("db error: %v", err)
		return "", http.StatusInternalServerError, fmt.Errorf("db error: %v", err)
	}

	return jiaUserID, 0, nil
}

func getJIAServiceURL(tx *sqlx.Tx) string {
	var config Config
	err := tx.Get(
		&config,
		"SELECT * FROM `isu_association_config` WHERE `name` = ?",
		"jia_service_url",
	)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			log.Print(err)
		}
		return defaultJIAServiceURL
	}
	return config.URL
}

// POST /initialize
// サービスを初期化
func postInitialize(c echo.Context) error {
	var request InitializeRequest
	err := c.Bind(&request)
	if err != nil {
		return c.String(http.StatusBadRequest, "bad request body")
	}

	cmd := exec.Command("../sql/init.sh")
	cmd.Stderr = os.Stderr
	cmd.Stdout = os.Stderr
	err = cmd.Run()
	if err != nil {
		c.Logger().Errorf("exec init.sh error: %v", err)
		return c.NoContent(http.StatusInternalServerError)
	}

	_, err = db.Exec(
		"INSERT INTO `isu_association_config` (`name`, `url`) VALUES (?, ?) ON DUPLICATE KEY UPDATE `url` = VALUES(`url`)",
		"jia_service_url",
		request.JIAServiceURL,
	)
	if err != nil {
		c.Logger().Errorf("db error : %v", err)
		return c.NoContent(http.StatusInternalServerError)
	}

	conds := []IsuCondition{}
	err = db.Select(
		&conds,
		"SELECT `jia_isu_uuid`, `timestamp`, `is_sitting`, `condition`, `message`, `level` FROM `isu_condition`",
	)
	if err != nil {
		c.Logger().Errorf("db error : %v", err)
		return c.NoContent(http.StatusInternalServerError)
	}
	for _, cond := range conds {
		cond.Level, err = calculateConditionLevel(cond.Condition)
		if err != nil {
			c.Logger().Errorf("failed to calculate condition level: %v", err)
			return c.NoContent(http.StatusInternalServerError)
		}
		_, err = db.Exec(
			"UPDATE `isu_condition` SET `level` = ? WHERE `jia_isu_uuid` = ? AND `timestamp` = ?",
			cond.Level,
			cond.JIAIsuUUID,
			cond.Timestamp,
		)
		if err != nil {
			c.Logger().Errorf("db error : %v", err)
			return c.NoContent(http.StatusInternalServerError)
		}
	}

	return c.JSON(http.StatusOK, InitializeResponse{
		Language: "go",
	})
}

// POST /api/auth
// サインアップ・サインイン
func postAuthentication(c echo.Context) error {
	reqJwt := strings.TrimPrefix(c.Request().Header.Get("Authorization"), "Bearer ")

	token, err := jwt.Parse(reqJwt, func(token *jwt.Token) (interface{}, error) {
		if _, ok := token.Method.(*jwt.SigningMethodECDSA); !ok {
			return nil, jwt.NewValidationError(
				fmt.Sprintf("unexpected signing method: %v", token.Header["alg"]),
				jwt.ValidationErrorSignatureInvalid,
			)
		}
		return jiaJWTSigningKey, nil
	})
	if err != nil {
		switch err.(type) {
		case *jwt.ValidationError:
			return c.String(http.StatusForbidden, "forbidden")
		default:
			c.Logger().Error(err)
			return c.NoContent(http.StatusInternalServerError)
		}
	}

	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok {
		c.Logger().Errorf("invalid JWT payload")
		return c.NoContent(http.StatusInternalServerError)
	}
	jiaUserIDVar, ok := claims["jia_user_id"]
	if !ok {
		return c.String(http.StatusBadRequest, "invalid JWT payload")
	}
	jiaUserID, ok := jiaUserIDVar.(string)
	if !ok {
		return c.String(http.StatusBadRequest, "invalid JWT payload")
	}

	_, err = db.Exec("INSERT IGNORE INTO user (`jia_user_id`) VALUES (?)", jiaUserID)
	if err != nil {
		c.Logger().Errorf("db error: %v", err)
		return c.NoContent(http.StatusInternalServerError)
	}

	sess, err := session.Get(sessionName, c)
	if err != nil {
		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}

	sess.Values["jia_user_id"] = jiaUserID
	sess.Options.Secure = false
	sess.Options.HttpOnly = true
	sess.Options.SameSite = http.SameSiteLaxMode
	err = sess.Save(c.Request(), c.Response())
	if err != nil {
		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}

	return c.NoContent(http.StatusOK)
}

// POST /api/signout
// サインアウト
func postSignout(c echo.Context) error {
	_, errStatusCode, err := getUserIDFromSession(c)
	if err != nil {
		if errStatusCode == http.StatusUnauthorized {
			return c.String(http.StatusUnauthorized, "you are not signed in")
		}

		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}

	sess, err := session.Get(sessionName, c)
	if err != nil {
		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}

	sess.Options = &sessions.Options{MaxAge: -1, Path: "/"}
	sess.Options.Secure = false
	sess.Options.HttpOnly = true
	sess.Options.SameSite = http.SameSiteLaxMode
	err = sess.Save(c.Request(), c.Response())
	if err != nil {
		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}

	return c.NoContent(http.StatusOK)
}

// GET /api/user/me
// サインインしている自分自身の情報を取得
func getMe(c echo.Context) error {
	jiaUserID, errStatusCode, err := getUserIDFromSession(c)
	if err != nil {
		if errStatusCode == http.StatusUnauthorized {
			return c.String(http.StatusUnauthorized, "you are not signed in")
		}

		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}

	res := GetMeResponse{JIAUserID: jiaUserID}
	return c.JSON(http.StatusOK, res)
}

// GET /api/isu
// ISUの一覧を取得
func getIsuList(c echo.Context) error {
	jiaUserID, errStatusCode, err := getUserIDFromSession(c)
	if err != nil {
		if errStatusCode == http.StatusUnauthorized {
			return c.String(http.StatusUnauthorized, "you are not signed in")
		}

		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}

	// tx, err := db.Beginx()
	// if err != nil {
	// 	c.Logger().Errorf("db error: %v", err)
	// 	return c.NoContent(http.StatusInternalServerError)
	// }
	// defer tx.Rollback()
	//

	stmt := "SELECT `id`, `jia_isu_uuid`, `name`, `character` FROM `isu` WHERE `jia_user_id` = ? ORDER BY `id` DESC"

	isuList := []Isu{}

	err = db.Select(&isuList, stmt, jiaUserID)
	if err != nil {
		c.Logger().Errorf("db error: %v", err)
		return c.NoContent(http.StatusInternalServerError)
	}

	responseList := make([]GetIsuListResponse, 0, len(isuList))
	found := true
	for _, isu := range isuList {
		lastCondition, err := isuConditionCache.Get(isu.JIAIsuUUID)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				found = false
			} else {
				return c.NoContent(http.StatusInternalServerError)
			}
		}
		var formattedCondition *GetIsuConditionResponse
		if found {
			formattedCondition = &GetIsuConditionResponse{
				JIAIsuUUID:     lastCondition.JIAIsuUUID,
				IsuName:        isu.Name,
				Timestamp:      lastCondition.Timestamp.Unix(),
				IsSitting:      lastCondition.IsSitting,
				Condition:      lastCondition.Condition,
				ConditionLevel: lastCondition.Level,
				Message:        lastCondition.Message,
			}
		}

		res := GetIsuListResponse{
			ID:                 isu.ID,
			JIAIsuUUID:         isu.JIAIsuUUID,
			Name:               isu.Name,
			Character:          isu.Character,
			LatestIsuCondition: formattedCondition,
		}
		responseList = append(responseList, res)
	}

	// err = tx.Commit()
	// if err != nil {
	// 	c.Logger().Errorf("db error: %v", err)
	// 	return c.NoContent(http.StatusInternalServerError)
	// }

	return c.JSON(http.StatusOK, responseList)
}

// POST /api/isu
// ISUを登録
func postIsu(c echo.Context) error {
	jiaUserID, errStatusCode, err := getUserIDFromSession(c)
	if err != nil {
		if errStatusCode == http.StatusUnauthorized {
			return c.String(http.StatusUnauthorized, "you are not signed in")
		}

		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}

	useDefaultImage := false

	jiaIsuUUID := c.FormValue("jia_isu_uuid")
	isuName := c.FormValue("isu_name")
	fh, err := c.FormFile("image")
	if err != nil {
		if !errors.Is(err, http.ErrMissingFile) {
			return c.String(http.StatusBadRequest, "bad format: icon")
		}
		useDefaultImage = true
	}

	var image []byte

	if useDefaultImage {
		image = defaultIcon
	} else {
		file, err := fh.Open()
		if err != nil {
			c.Logger().Error(err)
			return c.NoContent(http.StatusInternalServerError)
		}
		defer file.Close()

		image, err = io.ReadAll(file)
		if err != nil {
			c.Logger().Error(err)
			return c.NoContent(http.StatusInternalServerError)
		}
	}

	tx, err := db.Beginx()
	if err != nil {
		c.Logger().Errorf("db error: %v", err)
		return c.NoContent(http.StatusInternalServerError)
	}
	defer tx.Rollback()

	_, err = tx.Exec("INSERT INTO `isu`"+
		"	(`jia_isu_uuid`, `name`, `image`, `jia_user_id`) VALUES (?, ?, ?, ?)",
		jiaIsuUUID, isuName, image, jiaUserID)
	if err != nil {
		mysqlErr, ok := err.(*mysql.MySQLError)

		if ok && mysqlErr.Number == uint16(mysqlErrNumDuplicateEntry) {
			return c.String(http.StatusConflict, "duplicated: isu")
		}

		c.Logger().Errorf("db error: %v", err)
		return c.NoContent(http.StatusInternalServerError)
	}

	targetURL := getJIAServiceURL(tx) + "/api/activate"
	body := JIAServiceRequest{postIsuConditionTargetBaseURL, jiaIsuUUID}
	bodysonic, err := json.Marshal(body)
	if err != nil {
		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}

	reqJIA, err := http.NewRequest(http.MethodPost, targetURL, bytes.NewBuffer(bodysonic))
	if err != nil {
		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}

	reqJIA.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(reqJIA)
	if err != nil {
		return c.NoContent(http.StatusInternalServerError)
	}
	defer res.Body.Close()

	resBody, err := io.ReadAll(res.Body)
	if err != nil {
		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}

	if res.StatusCode != http.StatusAccepted {
		c.Logger().
			Errorf("JIAService returned error: status code %v, message: %v", res.StatusCode, string(resBody))
		return c.String(res.StatusCode, "JIAService returned error")
	}

	var isuFromJIA IsuFromJIA
	err = json.Unmarshal(resBody, &isuFromJIA)
	if err != nil {
		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}

	_, err = tx.Exec(
		"UPDATE `isu` SET `character` = ? WHERE  `jia_isu_uuid` = ?",
		isuFromJIA.Character,
		jiaIsuUUID,
	)
	if err != nil {
		c.Logger().Errorf("db error: %v", err)
		return c.NoContent(http.StatusInternalServerError)
	}

	var isu Isu
	err = tx.Get(
		&isu,
		"SELECT `id`, `jia_isu_uuid`, `name`, `character`, `jia_user_id` FROM `isu` WHERE `jia_user_id` = ? AND `jia_isu_uuid` = ?",
		jiaUserID,
		jiaIsuUUID,
	)
	if err != nil {
		c.Logger().Errorf("db error: %v", err)
		return c.NoContent(http.StatusInternalServerError)
	}

	err = tx.Commit()
	if err != nil {
		c.Logger().Errorf("db error: %v", err)
		return c.NoContent(http.StatusInternalServerError)
	}

	isuCache.Forget(jiaIsuUUID)
	return c.JSON(http.StatusCreated, isu)
}

// GET /api/isu/:jia_isu_uuid
// ISUの情報を取得
func getIsuID(c echo.Context) error {
	jiaUserID, errStatusCode, err := getUserIDFromSession(c)
	if err != nil {
		if errStatusCode == http.StatusUnauthorized {
			return c.String(http.StatusUnauthorized, "you are not signed in")
		}

		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}

	jiaIsuUUID := c.Param("jia_isu_uuid")

	// var res Isu
	// err = db.Get(&res, "SELECT * FROM `isu` WHERE `jia_user_id` = ? AND `jia_isu_uuid` = ?",
	// 	jiaUserID, jiaIsuUUID)
	// if err != nil {
	// 	if errors.Is(err, sql.ErrNoRows) {
	// 		return c.String(http.StatusNotFound, "not found: isu")
	// 	}
	//
	// 	c.Logger().Errorf("db error: %v", err)
	// 	return c.NoContent(http.StatusInternalServerError)
	// }

	isu, err := isuCache.Get(jiaIsuUUID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return c.String(http.StatusNotFound, "not found: isu")
		}
		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}

	if isu.JIAUserID != jiaUserID {
		return c.String(http.StatusNotFound, "not found: isu")
	}
	return c.JSON(http.StatusOK, isu)
}

// GET /api/isu/:jia_isu_uuid/icon
// ISUのアイコンを取得
func getIsuIcon(c echo.Context) error {
	jiaUserID, errStatusCode, err := getUserIDFromSession(c)
	if err != nil {
		if errStatusCode == http.StatusUnauthorized {
			return c.String(http.StatusUnauthorized, "you are not signed in")
		}

		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}

	jiaIsuUUID := c.Param("jia_isu_uuid")

	isu, err := isuCache.Get(jiaIsuUUID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return c.String(http.StatusNotFound, "not found: isu")
		}

		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}

	if isu.JIAUserID != jiaUserID {
		return c.String(http.StatusNotFound, "not found: isu")
	}

	return c.Blob(http.StatusOK, "", isu.Image)
}

// GET /api/isu/:jia_isu_uuid/graph
// ISUのコンディショングラフ描画のための情報を取得
func getIsuGraph(c echo.Context) error {
	jiaUserID, errStatusCode, err := getUserIDFromSession(c)
	if err != nil {
		if errStatusCode == http.StatusUnauthorized {
			return c.String(http.StatusUnauthorized, "you are not signed in")
		}

		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}

	jiaIsuUUID := c.Param("jia_isu_uuid")
	datetimeStr := c.QueryParam("datetime")
	if datetimeStr == "" {
		return c.String(http.StatusBadRequest, "missing: datetime")
	}
	datetimeInt64, err := strconv.ParseInt(datetimeStr, 10, 64)
	if err != nil {
		return c.String(http.StatusBadRequest, "bad format: datetime")
	}
	date := time.Unix(datetimeInt64, 0).Truncate(time.Hour)

	// tx, err := db.Beginx()
	// if err != nil {
	// 	c.Logger().Errorf("db error: %v", err)
	// 	return c.NoContent(http.StatusInternalServerError)
	// }
	// defer tx.Rollback()

	// var count int
	// err = tx.Get(
	// 	&count,
	// 	"SELECT COUNT(*) FROM `isu` WHERE `jia_user_id` = ? AND `jia_isu_uuid` = ?",
	// 	jiaUserID,
	// 	jiaIsuUUID,
	// )
	// if err != nil {
	// 	c.Logger().Errorf("db error: %v", err)
	// 	return c.NoContent(http.StatusInternalServerError)
	// }
	// if count == 0 {
	// 	return c.String(http.StatusNotFound, "not found: isu")
	// }

	isu, err := isuCache.Get(jiaIsuUUID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return c.String(http.StatusNotFound, "not found: isu")
		}

		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}

	if isu.JIAUserID != jiaUserID {
		return c.String(http.StatusNotFound, "not found: isu")
	}

	res, err := generateIsuGraphResponse(jiaIsuUUID, date)
	if err != nil {
		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}

	// err = tx.Commit()
	// if err != nil {
	// 	c.Logger().Errorf("db error: %v", err)
	// 	return c.NoContent(http.StatusInternalServerError)
	// }
	//
	return c.JSON(http.StatusOK, res)
}

// グラフのデータ点を一日分生成
func generateIsuGraphResponse(
	// tx *sqlx.Tx,
	jiaIsuUUID string,
	graphDate time.Time,
) ([]GraphResponse, error) {
	dataPoints := []GraphDataPointWithInfo{}
	conditionsInThisHour := []IsuCondition{}
	timestampsInThisHour := []int64{}
	var startTimeInThisHour time.Time
	var condition IsuCondition

	rows, err := db.Queryx(
		"SELECT `jia_isu_uuid`, `timestamp`, `is_sitting`, `condition`, `message`, `level` FROM `isu_condition` WHERE `jia_isu_uuid` = ? AND ? <= timestamp AND timestamp < ? ORDER BY `timestamp` ASC",
		jiaIsuUUID,
		graphDate,
		graphDate.Add(time.Hour*24),
	)
	if err != nil {
		return nil, fmt.Errorf("db error: %v", err)
	}

	for rows.Next() {
		err = rows.StructScan(&condition)
		if err != nil {
			return nil, err
		}

		truncatedConditionTime := condition.Timestamp.Truncate(time.Hour)
		if truncatedConditionTime != startTimeInThisHour {
			if len(conditionsInThisHour) > 0 {
				data, err := calculateGraphDataPoint(conditionsInThisHour)
				if err != nil {
					return nil, err
				}

				dataPoints = append(dataPoints,
					GraphDataPointWithInfo{
						JIAIsuUUID:          jiaIsuUUID,
						StartAt:             startTimeInThisHour,
						Data:                data,
						ConditionTimestamps: timestampsInThisHour,
					})
			}

			startTimeInThisHour = truncatedConditionTime
			conditionsInThisHour = []IsuCondition{}
			timestampsInThisHour = []int64{}
		}
		conditionsInThisHour = append(conditionsInThisHour, condition)
		timestampsInThisHour = append(timestampsInThisHour, condition.Timestamp.Unix())
	}

	if len(conditionsInThisHour) > 0 {
		data, err := calculateGraphDataPoint(conditionsInThisHour)
		if err != nil {
			return nil, err
		}

		dataPoints = append(dataPoints,
			GraphDataPointWithInfo{
				JIAIsuUUID:          jiaIsuUUID,
				StartAt:             startTimeInThisHour,
				Data:                data,
				ConditionTimestamps: timestampsInThisHour,
			})
	}

	endTime := graphDate.Add(time.Hour * 24)
	startIndex := len(dataPoints)
	endNextIndex := len(dataPoints)
	for i, graph := range dataPoints {
		if startIndex == len(dataPoints) && !graph.StartAt.Before(graphDate) {
			startIndex = i
		}
		if endNextIndex == len(dataPoints) && graph.StartAt.After(endTime) {
			endNextIndex = i
		}
	}

	filteredDataPoints := []GraphDataPointWithInfo{}
	if startIndex < endNextIndex {
		filteredDataPoints = dataPoints[startIndex:endNextIndex]
	}

	responseList := []GraphResponse{}
	index := 0
	thisTime := graphDate

	for thisTime.Before(graphDate.Add(time.Hour * 24)) {
		var data *GraphDataPoint
		timestamps := []int64{}

		if index < len(filteredDataPoints) {
			dataWithInfo := filteredDataPoints[index]

			if dataWithInfo.StartAt.Equal(thisTime) {
				data = &dataWithInfo.Data
				timestamps = dataWithInfo.ConditionTimestamps
				index++
			}
		}

		resp := GraphResponse{
			StartAt:             thisTime.Unix(),
			EndAt:               thisTime.Add(time.Hour).Unix(),
			Data:                data,
			ConditionTimestamps: timestamps,
		}
		responseList = append(responseList, resp)

		thisTime = thisTime.Add(time.Hour)
	}

	return responseList, nil
}

// 複数のISUのコンディションからグラフの一つのデータ点を計算
func calculateGraphDataPoint(isuConditions []IsuCondition) (GraphDataPoint, error) {
	conditionsCount := map[string]int{"is_broken": 0, "is_dirty": 0, "is_overweight": 0}
	rawScore := 0
	for _, condition := range isuConditions {
		badConditionsCount := 0

		if !isValidConditionFormat(condition.Condition) {
			return GraphDataPoint{}, fmt.Errorf("invalid condition format")
		}

		for _, condStr := range strings.Split(condition.Condition, ",") {
			keyValue := strings.Split(condStr, "=")

			conditionName := keyValue[0]
			if keyValue[1] == "true" {
				conditionsCount[conditionName] += 1
				badConditionsCount++
			}
		}

		if badConditionsCount >= 3 {
			rawScore += scoreConditionLevelCritical
		} else if badConditionsCount >= 1 {
			rawScore += scoreConditionLevelWarning
		} else {
			rawScore += scoreConditionLevelInfo
		}
	}

	sittingCount := 0
	for _, condition := range isuConditions {
		if condition.IsSitting {
			sittingCount++
		}
	}

	isuConditionsLength := len(isuConditions)

	score := rawScore * 100 / 3 / isuConditionsLength

	sittingPercentage := sittingCount * 100 / isuConditionsLength
	isBrokenPercentage := conditionsCount["is_broken"] * 100 / isuConditionsLength
	isOverweightPercentage := conditionsCount["is_overweight"] * 100 / isuConditionsLength
	isDirtyPercentage := conditionsCount["is_dirty"] * 100 / isuConditionsLength

	dataPoint := GraphDataPoint{
		Score: score,
		Percentage: ConditionsPercentage{
			Sitting:      sittingPercentage,
			IsBroken:     isBrokenPercentage,
			IsOverweight: isOverweightPercentage,
			IsDirty:      isDirtyPercentage,
		},
	}
	return dataPoint, nil
}

// GET /api/condition/:jia_isu_uuid
// ISUのコンディションを取得
func getIsuConditions(c echo.Context) error {
	jiaUserID, errStatusCode, err := getUserIDFromSession(c)
	if err != nil {
		if errStatusCode == http.StatusUnauthorized {
			return c.String(http.StatusUnauthorized, "you are not signed in")
		}

		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}

	jiaIsuUUID := c.Param("jia_isu_uuid")
	if jiaIsuUUID == "" {
		return c.String(http.StatusBadRequest, "missing: jia_isu_uuid")
	}

	endTimeInt64, err := strconv.ParseInt(c.QueryParam("end_time"), 10, 64)
	if err != nil {
		return c.String(http.StatusBadRequest, "bad format: end_time")
	}
	endTime := time.Unix(endTimeInt64, 0)
	conditionLevelCSV := c.QueryParam("condition_level")
	if conditionLevelCSV == "" {
		return c.String(http.StatusBadRequest, "missing: condition_level")
	}
	conditionLevel := map[string]interface{}{}
	for _, level := range strings.Split(conditionLevelCSV, ",") {
		conditionLevel[level] = struct{}{}
	}

	startTimeStr := c.QueryParam("start_time")
	var startTime time.Time
	if startTimeStr != "" {
		startTimeInt64, err := strconv.ParseInt(startTimeStr, 10, 64)
		if err != nil {
			return c.String(http.StatusBadRequest, "bad format: start_time")
		}
		startTime = time.Unix(startTimeInt64, 0)
	}

	var isuName string
	err = db.Get(&isuName,
		"SELECT name FROM `isu` WHERE `jia_isu_uuid` = ? AND `jia_user_id` = ?",
		jiaIsuUUID, jiaUserID,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return c.String(http.StatusNotFound, "not found: isu")
		}

		c.Logger().Errorf("db error: %v", err)
		return c.NoContent(http.StatusInternalServerError)
	}

	conditionsResponse, err := getIsuConditionsFromDB(
		db,
		jiaIsuUUID,
		endTime,
		conditionLevel,
		startTime,
		conditionLimit,
		isuName,
	)
	if err != nil {
		c.Logger().Errorf("db error: %v", err)
		return c.NoContent(http.StatusInternalServerError)
	}
	return c.JSON(http.StatusOK, conditionsResponse)
}

// ISUのコンディションをDBから取得
func getIsuConditionsFromDB(
	db *sqlx.DB,
	jiaIsuUUID string,
	endTime time.Time,
	conditionLevel map[string]interface{},
	startTime time.Time,
	limit int,
	isuName string,
) ([]*GetIsuConditionResponse, error) {
	conditions := []IsuCondition{}

	levels := maps.Keys(conditionLevel)
	if startTime.IsZero() {
		q, args, err := sqlx.In(
			"SELECT `jia_isu_uuid`, `timestamp`, `is_sitting`, `condition`, `message`, `level`  FROM `isu_condition` WHERE `jia_isu_uuid` = ?"+
				"	AND `timestamp` < ?"+
				"	AND `level` IN (?) "+
				"	ORDER BY `timestamp` DESC "+
				"	LIMIT ?",
			jiaIsuUUID,
			endTime,
			levels,
			limit,
		)
		if err != nil {
			return nil, fmt.Errorf("db error: %v", err)
		}
		q = db.Rebind(q)
		err = db.Select(&conditions, q, args...)
		if err != nil {
			return nil, fmt.Errorf("db error: %v", err)
		}
	} else {
		q, args, err := sqlx.In(
			"SELECT `jia_isu_uuid`, `timestamp`, `is_sitting`, `condition`, `message`, `level`  FROM `isu_condition` WHERE `jia_isu_uuid` = ?"+
				"	AND `timestamp` < ?"+
				"	AND ? <= `timestamp`"+
				"	AND `level` IN (?) "+
				"	ORDER BY `timestamp` DESC "+
				"	LIMIT ?",
			jiaIsuUUID, endTime, startTime, levels, limit,
		)
		if err != nil {
			return nil, fmt.Errorf("db error: %v", err)
		}
		q = db.Rebind(q)
		err = db.Select(&conditions, q, args...)
		if err != nil {
			return nil, fmt.Errorf("db error: %v", err)
		}
	}

	conditionsResponse := []*GetIsuConditionResponse{}
	for _, c := range conditions {
		cLevel := c.Level
		data := GetIsuConditionResponse{
			JIAIsuUUID:     c.JIAIsuUUID,
			IsuName:        isuName,
			Timestamp:      c.Timestamp.Unix(),
			IsSitting:      c.IsSitting,
			Condition:      c.Condition,
			ConditionLevel: cLevel,
			Message:        c.Message,
		}
		conditionsResponse = append(conditionsResponse, &data)
	}

	if len(conditionsResponse) > limit {
		conditionsResponse = conditionsResponse[:limit]
	}

	return conditionsResponse, nil
}

// ISUのコンディションの文字列からコンディションレベルを計算
func calculateConditionLevel(condition string) (string, error) {
	var conditionLevel string

	warnCount := strings.Count(condition, "=true")
	switch warnCount {
	case 0:
		conditionLevel = conditionLevelInfo
	case 1, 2:
		conditionLevel = conditionLevelWarning
	case 3:
		conditionLevel = conditionLevelCritical
	default:
		return "", fmt.Errorf("unexpected warn count")
	}

	return conditionLevel, nil
}

// GET /api/trend
// ISUの性格毎の最新のコンディション情報
func getTrend(c echo.Context) error {
	res := trendCache.Get()
	return c.JSON(http.StatusOK, res)
}

func calculateTrend() []TrendResponse {
	characterList := []Isu{}
	err := db.Select(&characterList, "SELECT `character` FROM `isu` GROUP BY `character`")
	if err != nil {
		log.Errorf("db error: %v", err)
		return nil
	}

	res := []TrendResponse{}

	for _, character := range characterList {
		isuList := []Isu{}
		err = db.Select(
			&isuList,
			"SELECT `id`, `jia_isu_uuid` FROM `isu` WHERE `character` = ?",
			character.Character,
		)
		if err != nil {
			log.Errorf("db error: %v", err)
			return nil
		}

		characterInfoIsuConditions := []*TrendCondition{}
		characterWarningIsuConditions := []*TrendCondition{}
		characterCriticalIsuConditions := []*TrendCondition{}

		for _, isu := range isuList {
			cond, err := isuConditionCache.Get(isu.JIAIsuUUID)
			if err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					continue
				} else {
					log.Errorf("db error: %v", err)
					return nil
				}
			}

			conditionLevel := cond.Level
			trendCondition := TrendCondition{
				ID:        isu.ID,
				Timestamp: cond.Timestamp.Unix(),
			}
			switch conditionLevel {
			case "info":
				characterInfoIsuConditions = append(characterInfoIsuConditions, &trendCondition)
			case "warning":
				characterWarningIsuConditions = append(
					characterWarningIsuConditions,
					&trendCondition,
				)
			case "critical":
				characterCriticalIsuConditions = append(
					characterCriticalIsuConditions,
					&trendCondition,
				)
			}
		}

		sort.Slice(characterInfoIsuConditions, func(i, j int) bool {
			return characterInfoIsuConditions[i].Timestamp > characterInfoIsuConditions[j].Timestamp
		})
		sort.Slice(characterWarningIsuConditions, func(i, j int) bool {
			return characterWarningIsuConditions[i].Timestamp > characterWarningIsuConditions[j].Timestamp
		})
		sort.Slice(characterCriticalIsuConditions, func(i, j int) bool {
			return characterCriticalIsuConditions[i].Timestamp > characterCriticalIsuConditions[j].Timestamp
		})

		res = append(res,
			TrendResponse{
				Character: character.Character,
				Info:      characterInfoIsuConditions,
				Warning:   characterWarningIsuConditions,
				Critical:  characterCriticalIsuConditions,
			})
	}

	return res
}

func calculateTrendScheduled(interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			trend := calculateTrend()
			trendCache.Set(trend)
		}
	}
}

// POST /api/condition/:jia_isu_uuid
// ISUからのコンディションを受け取る
func postIsuCondition(c echo.Context) error {
	// TODO: 一定割合リクエストを落としてしのぐようにしたが、本来は全量さばけるようにすべき
	// dropProbability := 0.3
	// if rand.Float64() <= dropProbability {
	// 	c.Logger().Warnf("drop post isu condition request")
	// 	return c.NoContent(http.StatusAccepted)
	// }
	//
	jiaIsuUUID := c.Param("jia_isu_uuid")
	if jiaIsuUUID == "" {
		return c.String(http.StatusBadRequest, "missing: jia_isu_uuid")
	}

	req := []PostIsuConditionRequest{}
	err := c.Bind(&req)
	if err != nil {
		return c.String(http.StatusBadRequest, "bad request body")
	} else if len(req) == 0 {
		return c.String(http.StatusBadRequest, "bad request body")
	}
	//
	// tx, err := db.Beginx()
	// if err != nil {
	// 	c.Logger().Errorf("db error: %v", err)
	// 	return c.NoContent(http.StatusInternalServerError)
	// }
	// defer tx.Rollback()

	// var count int
	// err = db.Get(&count, "SELECT 1 FROM `isu` WHERE `jia_isu_uuid` = ? LIMIT 1", jiaIsuUUID)
	// if err != nil {
	// 	c.Logger().Errorf("db error: %v", err)
	// 	return c.NoContent(http.StatusInternalServerError)
	// }
	_, err = isuCache.Get(jiaIsuUUID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return c.String(http.StatusNotFound, "not found: isu")
		}
		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}
	// if count == 0 {
	// 	return c.String(http.StatusNotFound, "not found: isu")
	// }

	conds := make([]IsuCondition, 0, len(req))

	for _, cond := range req {
		timestamp := time.Unix(cond.Timestamp, 0)

		if !isValidConditionFormat(cond.Condition) {
			return c.String(http.StatusBadRequest, "bad request body")
		}
		level, err := calculateConditionLevel(cond.Condition)
		if err != nil {
			c.Logger().Error(err)
			return c.NoContent(http.StatusInternalServerError)
		}
		conds = append(conds, IsuCondition{
			JIAIsuUUID: jiaIsuUUID,
			Timestamp:  timestamp,
			IsSitting:  cond.IsSitting,
			Condition:  cond.Condition,
			Message:    cond.Message,
			Level:      level,
		})
	}
	insertQueue.Insert(conds)
	// _, err = tx.NamedExec("INSERT INTO `isu_condition`"+
	// 	"	(`jia_isu_uuid`, `timestamp`, `is_sitting`, `condition`, `message`)"+
	// 	"	VALUES (:jia_isu_uuid, :timestamp, :is_sitting, :condition, :message)", conds)
	// if err != nil {
	// 	c.Logger().Errorf("db error: %v", err)
	// 	return c.NoContent(http.StatusInternalServerError)
	// }
	//
	// err = tx.Commit()
	// if err != nil {
	// 	c.Logger().Errorf("db error: %v", err)
	// 	return c.NoContent(http.StatusInternalServerError)
	// }

	return c.NoContent(http.StatusAccepted)
}

// ISUのコンディションの文字列がcsv形式になっているか検証
func isValidConditionFormat(conditionStr string) bool {
	keys := []string{"is_dirty=", "is_overweight=", "is_broken="}
	const valueTrue = "true"
	const valueFalse = "false"

	idxCondStr := 0

	for idxKeys, key := range keys {
		if !strings.HasPrefix(conditionStr[idxCondStr:], key) {
			return false
		}
		idxCondStr += len(key)

		if strings.HasPrefix(conditionStr[idxCondStr:], valueTrue) {
			idxCondStr += len(valueTrue)
		} else if strings.HasPrefix(conditionStr[idxCondStr:], valueFalse) {
			idxCondStr += len(valueFalse)
		} else {
			return false
		}

		if idxKeys < (len(keys) - 1) {
			if conditionStr[idxCondStr] != ',' {
				return false
			}
			idxCondStr++
		}
	}

	return (idxCondStr == len(conditionStr))
}

func insertIsuConditionScheduled(interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			q := insertQueue.PopAll()
			if len(q) == 0 {
				continue
			}

			for _, cond := range q {
				isuConditionCache.Forget(cond.JIAIsuUUID)
			}
			_, err := db.NamedExec("INSERT INTO `isu_condition`"+
				"	(`jia_isu_uuid`, `timestamp`, `is_sitting`, `condition`, `message`, `level`)"+
				"	VALUES (:jia_isu_uuid, :timestamp, :is_sitting, :condition, :message, :level)", q)
			if err != nil {
				log.Printf("failed to insert isu condition: %v", err)
			}
		}
	}
}

// func getIndex(c echo.Context) error {
// 	return c.File(frontendContentsPath + "/index.html")
// }
