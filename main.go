package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"text/template"
	"time"

	"github.com/bluesky-social/indigo/api/atproto"
	"github.com/bluesky-social/indigo/api/bsky"
	"github.com/bluesky-social/indigo/lex/util"
	"github.com/bluesky-social/indigo/xrpc"
	"github.com/go-co-op/gocron"
	"github.com/heetch/confita"
	confitaFile "github.com/heetch/confita/backend/file"
	"golang.org/x/xerrors"
)

const (
	ISO8601     = "2006-01-02T15:04:05.000Z"
	POST_FORMAT = `【{{ .Yesterday }}の統計】
ポスト数: {{ .PostsCount }}({{ formatDiff .PostsCountDiff }})
フォロー数: {{ .FollowsCount }}({{ formatDiff .FollowsCountDiff }})
フォロワー数: {{ .FollowersCount }}({{ formatDiff .FollowersCountDiff }}))`
)

type Config struct {
	Host     string `config:"host"`
	Handle   string `config:"handle"`
	Password string `config:"password"`
}

type Data struct {
	Posts     int64 `json:"posts"`
	Follows   int64 `json:"follows"`
	Followers int64 `json:"followers"`
}

type Param struct {
	Yesterday          string
	PostsCount         int64
	PostsCountDiff     int64
	FollowsCount       int64
	FollowsCountDiff   int64
	FollowersCount     int64
	FollowersCountDiff int64
}

func main() {
	ctx := context.Background()

	funcMap := template.FuncMap{
		"formatDiff": formatDiff,
	}

	tmpl, err := template.New("post").Funcs(funcMap).Parse(POST_FORMAT)
	if err != nil {
		log.Fatalf("failed to parse template: %+v", err)
	}

	loader := confita.NewLoader(
		confitaFile.NewBackend("config.json"),
	)

	cfg := new(Config)
	if err := loader.Load(ctx, cfg); err != nil {
		log.Fatalf("failed to load config: %+v", err)
	}

	client, err := newClient(ctx, cfg)
	if err != nil {
		log.Fatalf("failed to create client: %+v", err)
	}

	data, err := fetchData(ctx, client)
	if err != nil {
		log.Fatalf("failed to initialize data: %+v", err)
	}

	s := gocron.NewScheduler(time.Local)

	s.Every(1).Day().At("00:00").Do(func() {
		newData, err := fetchData(ctx, client)
		if err != nil {
			log.Printf("failed to update data: %+v\n", err)
			return
		}

		param := &Param{
			Yesterday:          time.Now().AddDate(0, 0, -1).Format("2006-01-02"),
			PostsCount:         data.Posts,
			PostsCountDiff:     newData.Posts - data.Posts,
			FollowsCount:       data.Follows,
			FollowsCountDiff:   newData.Follows - data.Follows,
			FollowersCount:     data.Followers,
			FollowersCountDiff: newData.Followers - data.Followers,
		}

		buf := new(bytes.Buffer)

		if err := tmpl.Execute(buf, param); err != nil {
			log.Printf("failed to execute template: %+v\n", err)
			return
		}

		if _, err := post(ctx, client, buf.String()); err != nil {
			log.Printf("failed to post: %+v\n", err)
			return
		}

		log.Println("post success")
	})

	log.Println("Starting...")
	s.StartBlocking()
}

func newClient(ctx context.Context, cfg *Config) (*xrpc.Client, error) {
	client := &xrpc.Client{
		Client: new(http.Client),
		Host:   cfg.Host,
		Auth:   &xrpc.AuthInfo{Handle: cfg.Handle},
	}

	b := sha256.Sum256([]byte(fmt.Sprintf("%s_%s", cfg.Host, cfg.Handle)))
	authFileName := fmt.Sprintf("auth_%s.json", hex.EncodeToString(b[:]))

	exists := existsFile(authFileName)

	file, err := os.Create(authFileName)
	if err != nil {
		return nil, xerrors.Errorf("failed to open auth file: %w", err)
	}

	defer file.Close()

	if exists {
		b, err := io.ReadAll(file)
		if err != nil {
			return nil, xerrors.Errorf("failed to read auth file: %w", err)
		}

		if err := json.Unmarshal(b, client.Auth); err != nil {
			return nil, xerrors.Errorf("failed to parse auth file: %w", err)
		}

		session, err := atproto.ServerRefreshSession(ctx, client)
		if err != nil {
			if err := createSession(ctx, client, cfg); err != nil {
				return nil, xerrors.Errorf("failed to create session: %w", err)
			}

			if err := saveSession(client.Auth, file); err != nil {
				return nil, xerrors.Errorf("failed to save session: %w", err)
			}

			return client, nil
		}

		client.Auth.Did = session.Did
		client.Auth.AccessJwt = session.AccessJwt
		client.Auth.RefreshJwt = session.RefreshJwt

		return client, nil
	}

	if err := createSession(ctx, client, cfg); err != nil {
		return nil, xerrors.Errorf("failed to create session: %w", err)
	}

	if err := saveSession(client.Auth, file); err != nil {
		return nil, xerrors.Errorf("failed to save session: %w", err)
	}

	return client, nil
}

func createSession(ctx context.Context, client *xrpc.Client, cfg *Config) error {
	session, err := atproto.ServerCreateSession(
		ctx, client, &atproto.ServerCreateSession_Input{
			Identifier: client.Auth.Handle,
			Password:   cfg.Password,
		},
	)
	if err != nil {
		return xerrors.Errorf("failed to create session: %w", err)
	}

	client.Auth.Did = session.Did
	client.Auth.AccessJwt = session.AccessJwt
	client.Auth.RefreshJwt = session.RefreshJwt

	return nil
}

func saveSession(auth *xrpc.AuthInfo, file *os.File) error {
	b, err := json.Marshal(auth)
	if err != nil {
		return xerrors.Errorf("failed to marshal auth: %w", err)
	}

	if _, err := file.Write(b); err != nil {
		return xerrors.Errorf("failed to write auth file: %w", err)
	}

	return nil
}

func existsFile(filename string) bool {
	_, err := os.Stat(filename)
	return err == nil
}

func fetchData(ctx context.Context, client *xrpc.Client) (Data, error) {
	profile, err := bsky.ActorGetProfile(ctx, client, client.Auth.Handle)
	if err != nil {
		return Data{}, xerrors.Errorf("failed to get profile: %w", err)
	}

	return Data{
		Posts:     *profile.PostsCount,
		Follows:   *profile.FollowsCount,
		Followers: *profile.FollowersCount,
	}, nil
}

func post(ctx context.Context, client *xrpc.Client, text string) (*atproto.RepoCreateRecord_Output, error) {
	return atproto.RepoCreateRecord(ctx, client, &atproto.RepoCreateRecord_Input{
		Collection: "app.bsky.feed.post",
		Repo:       client.Auth.Did,
		Record: &util.LexiconTypeDecoder{
			Val: &bsky.FeedPost{
				Text:      text,
				CreatedAt: time.Now().Format(ISO8601),
			},
		},
	})
}

func formatDiff(diff int64) string {
	if diff == 0 {
		return "±0"
	}

	if diff > 0 {
		return fmt.Sprintf("+%d", diff)
	}

	return fmt.Sprintf("%d", diff)
}
