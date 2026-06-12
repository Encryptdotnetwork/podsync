package main

import (
	"context"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jessevdk/go-flags"
	"github.com/mxpv/podsync/pkg/config"
	"github.com/mxpv/podsync/services/admin"
	"github.com/mxpv/podsync/services/migrate"
	"github.com/mxpv/podsync/services/update"
	"github.com/mxpv/podsync/services/web"
	log "github.com/sirupsen/logrus"
	"golang.org/x/sync/errgroup"
	"gopkg.in/natefinch/lumberjack.v2"

	"github.com/mxpv/podsync/pkg/db"
	"github.com/mxpv/podsync/pkg/fs"
	"github.com/mxpv/podsync/pkg/ytdl"
)

type Opts struct {
	ConfigPath             string `long:"config" short:"c" default:"config.toml" env:"PODSYNC_CONFIG_PATH"`
	Headless               bool   `long:"headless"`
	MigrateFilenames       bool   `long:"migrate-filenames" description:"Migrate existing downloaded filenames to current filename_template and exit"`
	MigrateFilenamesDryRun bool   `long:"migrate-filenames-dry-run" description:"Preview filename migration without writing changes (requires --migrate-filenames)"`
	Debug                  bool   `long:"debug"`
	NoBanner               bool   `long:"no-banner"`
}

const banner = `
 _______  _______  ______   _______           _        _______ 
(  ____ )(  ___  )(  __  \ (  ____ \|\     /|( (    /|(  ____ \
| (    )|| (   ) || (  \  )| (    \/( \   / )|  \  ( || (    \/
| (____)|| |   | || |   ) || (_____  \ (_) / |   \ | || |      
|  _____)| |   | || |   | |(_____  )  \   /  | (\ \) || |      
| (      | |   | || |   ) |      ) |   ) (   | | \   || |      
| )      | (___) || (__/  )/\____) |   | |   | )  \  || (____/\
|/       (_______)(______/ \_______)   \_/   |/    )_)(_______/
`

var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
	arch    = ""
)

func main() {
	log.SetFormatter(&log.TextFormatter{
		TimestampFormat: time.RFC3339,
		FullTimestamp:   true,
	})

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Parse args
	opts := Opts{}
	_, err := flags.Parse(&opts)
	if err != nil {
		log.WithError(err).Fatal("failed to parse command line arguments")
	}

	if opts.Debug {
		log.SetLevel(log.DebugLevel)
	}
	if opts.MigrateFilenamesDryRun && !opts.MigrateFilenames {
		log.Fatal("--migrate-filenames-dry-run requires --migrate-filenames")
	}

	if !opts.NoBanner {
		log.Info(banner)
	}

	log.WithFields(log.Fields{
		"version": version,
		"commit":  commit,
		"date":    date,
		"arch":    arch,
	}).Info("running podsync")

	// Load TOML file; the store owns config.toml from here on and publishes
	// hot-reload events consumed by the scheduler
	log.Debugf("loading configuration %q", opts.ConfigPath)
	store, err := config.NewStore(opts.ConfigPath)
	if err != nil {
		log.WithError(err).Fatal("failed to load configuration file")
	}
	cfg := store.Get()

	if cfg.Log.Filename != "" {
		log.Infof("Using log file: %s", cfg.Log.Filename)

		log.SetOutput(&lumberjack.Logger{
			Filename:   cfg.Log.Filename,
			MaxSize:    cfg.Log.MaxSize,
			MaxBackups: cfg.Log.MaxBackups,
			MaxAge:     cfg.Log.MaxAge,
			Compress:   cfg.Log.Compress,
		})

		// Optionally enable debug mode from config.toml
		if cfg.Log.Debug {
			log.SetLevel(log.DebugLevel)
		}
	}

	database, err := db.NewBadger(&cfg.Database)
	if err != nil {
		log.WithError(err).Fatal("failed to open database")
	}
	defer func() {
		if err := database.Close(); err != nil {
			log.WithError(err).Error("failed to close database")
		}
	}()

	var storage fs.Storage
	switch cfg.Storage.Type {
	case "local":
		storage, err = fs.NewLocal(cfg.Storage.Local.DataDir, cfg.Server.WebUIEnabled, cfg.Server.NoListing)
	case "s3":
		storage, err = fs.NewS3(cfg.Storage.S3) // serving files from S3 is not supported, so no WebUI either
	default:
		log.Fatalf("unknown storage type: %s", cfg.Storage.Type)
	}
	if err != nil {
		log.WithError(err).Fatal("failed to open storage")
	}

	if opts.MigrateFilenames {
		if cfg.Storage.Type == "s3" && !opts.MigrateFilenamesDryRun {
			log.Fatal("--migrate-filenames is not supported with storage.type = \"s3\"; use --migrate-filenames-dry-run or migrate with local storage")
		}

		migration := migrate.New(cfg.Feeds, database, storage, opts.MigrateFilenamesDryRun)
		result, err := migration.Run(ctx)
		if err != nil {
			log.WithError(err).Fatal("filename migration failed")
		}
		log.WithFields(log.Fields{
			"feeds":                   result.Feeds,
			"episodes":                result.Episodes,
			"migrated":                result.Migrated,
			"already_good":            result.AlreadyGood,
			"missing_old":             result.MissingOld,
			"skipped_existing_target": result.SkippedDueToExistingTarget,
			"dry_run":                 opts.MigrateFilenamesDryRun,
		}).Info("filename migration completed")
		return
	}

	downloader, err := ytdl.New(ctx, cfg.Downloader)
	if err != nil {
		log.WithError(err).Fatal("youtube-dl error")
	}

	// In Headless mode, do one round of feed updates and quit
	if opts.Headless {
		log.Debug("creating update manager")
		keys, err := update.KeyProviders(cfg.Tokens)
		if err != nil {
			log.WithError(err).Fatal("failed to create key providers")
		}

		manager, err := update.NewUpdater(cfg.Feeds, keys, cfg.Server.Hostname, downloader, database, storage)
		if err != nil {
			log.WithError(err).Fatal("failed to create updater")
		}

		for _, _feed := range cfg.Feeds {
			if err := manager.Update(ctx, _feed); err != nil {
				log.WithError(err).Errorf("failed to update feed: %s", _feed.URL)
			}
		}
		return
	}

	// The scheduler owns the update manager, the update queue, the cron table
	// and the config hot-reload loop
	log.Debug("creating update scheduler")
	scheduler, err := update.NewScheduler(store, downloader, database, storage)
	if err != nil {
		log.WithError(err).Fatal("failed to create scheduler")
	}
	defer scheduler.Close()

	group, ctx := errgroup.WithContext(ctx)
	defer func() {
		if err := group.Wait(); err != nil && (err != context.Canceled && err != http.ErrServerClosed) {
			log.WithError(err).Error("wait error")
		}
		log.Info("gracefully stopped")
	}()

	// Run the scheduler: feed updates, cron and config hot-reload
	group.Go(func() error {
		return scheduler.Run(ctx)
	})

	if cfg.Storage.Type == "s3" {
		return // S3 content is hosted externally
	}

	// Run web server
	srv := web.New(cfg.Server, storage, database)

	group.Go(func() error {
		log.Infof("running listener at %s", srv.Addr)
		if cfg.Server.TLS {
			return srv.ListenAndServeTLS(cfg.Server.CertificatePath, cfg.Server.KeyFilePath)
		} else {
			return srv.ListenAndServe()
		}
	})

	// Run internal admin API server (config management and hot reload)
	if cfg.Admin.Enabled {
		adminSrv := admin.New(cfg.Admin, store, func() error {
			_, err := store.Reload()
			return err
		})

		group.Go(func() error {
			log.Infof("running admin API at %s", adminSrv.Addr)
			return adminSrv.ListenAndServe()
		})

		group.Go(func() error {
			<-ctx.Done()

			ctxShutDown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			log.Info("shutting down admin server")
			if err := adminSrv.Shutdown(ctxShutDown); err != nil {
				log.WithError(err).Error("admin server shutdown failed")
			}
			return nil
		})
	}

	group.Go(func() error {
		// Shutdown web server
		defer func() {
			ctxShutDown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer func() {
				cancel()
			}()
			log.Info("shutting down web server")
			if err := srv.Shutdown(ctxShutDown); err != nil {
				log.WithError(err).Error("server shutdown failed")
			}
		}()

		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-stop:
				cancel()
				return nil
			}
		}
	})
}
