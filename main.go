package main

import (
	"context"
	"io"
	"log"
	"os"
	"path"
	"strings"
	"time"

	"filippo.io/age"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3/s3manager"
	"github.com/caarlos0/env"
	"github.com/redis/go-redis/v9"
)

type _opts struct {
	DatabaseString   string `env:"DATABASE_CONNECTIONSTRING" envDefault:"localhost:6379"`
	DatabasePassword string `env:"DATABASE_PASSWORD"`
	DatabaseUsername string `env:"DATABASE_USERNAME"`
	Frequency        string `env:"BACKUP_FREQUENCY"`
	PublicKey        string `env:"PUBLIC_KEY"`

	AWSRegion string `env:"AWS_REGION"`
	AWSBucket string `env:"AWS_BUCKET"`

	BucketDirectory string `env:"AWS_BUCKET_FOLDER"`

	RetryWaitTimeInSeconds int `env:"RETRY_WAIT_TIME_IN_SECONDS" envDefault:"10"`

	frequency time.Duration
}

func main() {
	ctx := context.Background()

	opts := &_opts{}
	if err := env.Parse(opts); err != nil {
		log.Fatalf("failed to parse environment for environment variables: %v", err)
	}

	var err error

	opts.frequency, err = time.ParseDuration(opts.Frequency)
	if err != nil {
		log.Fatalf("failed to parse backup frequency: %v", err)
	}

	defer func() {
		err := recover()
		if err == nil {
			return
		}

		_error, ok := err.(error)
		if !ok {
			panic(err)
		}

		if strings.Contains(_error.Error(), "bad state: EOF") {
			log.Printf("connection closed, reconnecting...\n")

			startLoop(ctx, opts)
		}

		panic(err)
	}()

	startLoop(ctx, opts)
}

func startLoop(ctx context.Context, opts *_opts) {
	conn, err := connect(ctx, opts)
	if err != nil {
		panic(err)
	}

	defer conn.Close()

	for {
		if yes, err := isMaster(ctx, conn); err != nil {
			panic(err)
		} else if !yes {
			log.Println("not a master node, skipping operation")
		} else {
			startBackup(ctx, conn, opts)
		}
		time.Sleep(opts.frequency)
	}
}

func startBackup(ctx context.Context, conn *redis.Conn, opts *_opts) {
	log.Println("running backup ...")

	last, err := conn.LastSave(ctx).Result()
	if err != nil {
		panic(err)
	}

	bgSave, err := conn.BgSave(ctx).Result()
	if err != nil {
		panic(err)
	}

	log.Printf("BGSAVE: %v\n", bgSave)

	waitTime := opts.RetryWaitTimeInSeconds // seconds
	for i := 0; i < 5; i++ {
		now, err := conn.LastSave(ctx).Result()
		if err != nil {
			panic(err)
		}

		if now == last {
			if i == 4 {
				// maybe something is wrong, better exit and check out
				panic("timed out waiting for bgsave to complete, try increasing RETRY_WAIT_TIME_IN_SECONDS value")
			}
			time.Sleep(time.Second * time.Duration(waitTime))
			waitTime += waitTime
			continue
		}

		log.Println("successfully backed up redis")
		dir, err := conn.ConfigGet(ctx, "dir").Result()
		if err != nil {
			panic(err)
		}

		var backupFile = dir["dir"] + "/dump.rdb"
		log.Printf("backup file: %s\n", backupFile)

		var agefile string
		if agefile, err = encrypt(opts.PublicKey, backupFile); err != nil {
			panic(err)
		}

		err = uploadToS3(agefile, opts)
		if err != nil {
			panic(err)
		}

		if err := os.Remove(agefile); err != nil {
			log.Printf("failed to delete encrypted backup file %v\n", agefile)
		}

		break
	}

	log.Printf("backup done, waiting %s for next..", opts.Frequency)
}

func connect(ctx context.Context, opts *_opts) (*redis.Conn, error) {
	conn := redis.NewClient(&redis.Options{
		Addr:     opts.DatabaseString,
		Username: opts.DatabaseUsername,
		Password: opts.DatabasePassword,
	}).Conn()

	if err := conn.ClientSetName(ctx, "redis-backup-sidecar").Err(); err != nil {
		return nil, err
	}
	return conn, nil
}

/*
* isMaster checks if current running redis instance is the master node or not
* backup only runs if current node is master
* this makes sure backup runs on a single sidecar at a time
* no master, oh you have a bigger problem to solve my friend
 */
func isMaster(ctx context.Context, connection *redis.Conn) (yes bool, err error) {
	v, err := connection.Info(ctx).Result()
	if err != nil {
		return
	}

	for _, line := range strings.Split(v, "\n") {
		if strings.Contains(line, "role:master") {
			yes = true
			break
		}
	}

	return
}

func uploadToS3(file string, opts *_opts) error {
	session := session.Must(session.NewSession(&aws.Config{
		Region: aws.String(opts.AWSRegion),
	}))

	// https://docs.aws.amazon.com/sdk-for-go/api/service/s3/
	s3 := s3manager.NewUploader(session)

	src, err := os.Open(file)
	if err != nil {
		return err
	}

	key := path.Join(opts.BucketDirectory, path.Base(file))

	result, err := s3.Upload(&s3manager.UploadInput{Bucket: aws.String(opts.AWSBucket), Key: aws.String(key), Body: src})
	if err != nil {
		return err
	}

	log.Printf("successfully uploaded backup to %v\n", result.Location)

	return src.Close()
}

func encrypt(key, file string) (string, error) {
	v, err := age.ParseX25519Recipient(key)
	if err != nil {
		return "", err
	}

	agefile := "dump-" + time.Now().Format(time.RFC3339) + ".rdb.age"

	dst, err := os.Create(agefile)
	if err != nil {
		return "", err
	}

	defer dst.Close()

	dst_w, err := age.Encrypt(dst, v)
	if err != nil {
		return "", err
	}

	src, err := os.Open(file)
	if err != nil {
		return "", err
	}

	if _, err := io.Copy(dst_w, src); err != nil {
		return "", err
	}

	return agefile, dst_w.Close()
}
