package upgrade

import (
	"context"
	"errors"
	"fmt"
	"io/ioutil"
	"net/url"
	"os"
	"os/user"
	"path/filepath"
	"strings"

	"github.com/influxdata/influxdb/v2"
	"github.com/influxdata/influxdb/v2/authorization"
	"github.com/influxdata/influxdb/v2/bolt"
	"github.com/influxdata/influxdb/v2/dbrp"
	"github.com/influxdata/influxdb/v2/fluxinit"
	"github.com/influxdata/influxdb/v2/internal/fs"
	"github.com/influxdata/influxdb/v2/kit/cli"
	"github.com/influxdata/influxdb/v2/kit/metric"
	"github.com/influxdata/influxdb/v2/kit/prom"
	"github.com/influxdata/influxdb/v2/kv"
	"github.com/influxdata/influxdb/v2/kv/migration"
	"github.com/influxdata/influxdb/v2/kv/migration/all"
	"github.com/influxdata/influxdb/v2/storage"
	"github.com/influxdata/influxdb/v2/tenant"
	authv1 "github.com/influxdata/influxdb/v2/v1/authorization"
	"github.com/influxdata/influxdb/v2/v1/services/meta"
	"github.com/influxdata/influxdb/v2/v1/services/meta/filestore"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// Simplified 1.x config.
type configV1 struct {
	Meta struct {
		Dir string `toml:"dir"`
	} `toml:"meta"`
	Data struct {
		Dir    string `toml:"dir"`
		WALDir string `toml:"wal-dir"`
	} `toml:"data"`
	Http struct {
		BindAddress  string `toml:"bind-address"`
		HttpsEnabled bool   `toml:"https-enabled"`
		AuthEnabled  bool   `toml:"auth-enabled"`
	} `toml:"http"`
}

func (c *configV1) dbURL() string {
	address := c.Http.BindAddress
	if address == "" { // fallback to default
		address = ":8086"
	}
	var url url.URL
	if c.Http.HttpsEnabled {
		url.Scheme = "https"
	} else {
		url.Scheme = "http"
	}
	if strings.HasPrefix(address, ":") { // address is just :port
		url.Host = "localhost" + address
	} else {
		url.Host = address
	}
	return url.String()
}

type optionsV1 struct {
	metaDir string
	walDir  string
	dataDir string
	dbURL   string
	// cmd option
	dbDir      string
	configFile string
}

// populateDirs sets values for expected sub-directories of o.dbDir
func (o *optionsV1) populateDirs() {
	o.metaDir = filepath.Join(o.dbDir, "meta")
	o.dataDir = filepath.Join(o.dbDir, "data")
	o.walDir = filepath.Join(o.dbDir, "wal")
}

type optionsV2 struct {
	boltPath       string
	cliConfigsPath string
	enginePath     string
	cqPath         string
	userName       string
	password       string
	orgName        string
	bucket         string
	orgID          influxdb.ID
	userID         influxdb.ID
	token          string
	retention      string
}

var options = struct {
	// flags for source InfluxDB
	source optionsV1

	// flags for target InfluxDB
	target optionsV2

	// verbose output
	verbose bool

	// logging
	logLevel string
	logPath  string

	force bool
}{}

func NewCommand(v *viper.Viper) *cobra.Command {

	// source flags
	v1dir, err := influxDirV1()
	if err != nil {
		panic("error fetching default InfluxDB 1.x dir: " + err.Error())
	}

	// target flags
	v2dir, err := fs.InfluxDir()
	if err != nil {
		panic("error fetching default InfluxDB 2.0 dir: " + err.Error())
	}

	cmd := &cobra.Command{
		Use:   "upgrade",
		Short: "Upgrade a 1.x version of InfluxDB",
		Long: `
    Upgrades a 1.x version of InfluxDB by performing the following actions:
      1. Reads the 1.x config file and creates a 2.x config file with matching options. Unsupported 1.x options are reported.
      2. Copies 1.x database files.
      3. Creates influx CLI configurations.
      4. Exports any 1.x continuous queries to disk.

    If the config file is not available, 1.x db folder (--v1-dir options) is taken as an input.
    Target 2.x database dir is specified by the --engine-path option. If changed, the bolt path should be changed as well.
`,
		RunE: runUpgradeE,
		Args: cobra.NoArgs,
	}

	opts := []cli.Opt{
		{
			DestP:   &options.source.dbDir,
			Flag:    "v1-dir",
			Default: v1dir,
			Desc:    "path to source 1.x db directory containing meta, data and wal sub-folders",
		},
		{
			DestP:   &options.verbose,
			Flag:    "verbose",
			Default: true,
			Desc:    "verbose output",
			Short:   'v',
		},
		{
			DestP:   &options.target.boltPath,
			Flag:    "bolt-path",
			Default: filepath.Join(v2dir, bolt.DefaultFilename),
			Desc:    "path for boltdb database",
			Short:   'm',
		},
		{
			DestP:   &options.target.cliConfigsPath,
			Flag:    "influx-configs-path",
			Default: filepath.Join(v2dir, "configs"),
			Desc:    "path for 2.x CLI configurations file",
			Short:   'c',
		},
		{
			DestP:   &options.target.enginePath,
			Flag:    "engine-path",
			Default: filepath.Join(v2dir, "engine"),
			Desc:    "path for persistent engine files",
			Short:   'e',
		},
		{
			DestP:   &options.target.cqPath,
			Flag:    "continuous-query-export-path",
			Default: filepath.Join(homeOrAnyDir(), "continuous_queries.txt"),
			Desc:    "path for exported 1.x continuous queries",
		},
		{
			DestP:    &options.target.userName,
			Flag:     "username",
			Default:  "",
			Desc:     "primary username",
			Short:    'u',
			Required: true,
		},
		{
			DestP:    &options.target.password,
			Flag:     "password",
			Default:  "",
			Desc:     "password for username",
			Short:    'p',
			Required: true,
		},
		{
			DestP:    &options.target.orgName,
			Flag:     "org",
			Default:  "",
			Desc:     "primary organization name",
			Short:    'o',
			Required: true,
		},
		{
			DestP:    &options.target.bucket,
			Flag:     "bucket",
			Default:  "",
			Desc:     "primary bucket name",
			Short:    'b',
			Required: true,
		},
		{
			DestP:   &options.target.retention,
			Flag:    "retention",
			Default: "",
			Desc:    "optional: duration bucket will retain data. 0 is infinite. The default is 0.",
			Short:   'r',
		},
		{
			DestP:   &options.target.token,
			Flag:    "token",
			Default: "",
			Desc:    "optional: token for username, else auto-generated",
			Short:   't',
		},
		{
			DestP:   &options.source.configFile,
			Flag:    "config-file",
			Default: influxConfigPathV1(),
			Desc:    "optional: Custom InfluxDB 1.x config file path, else the default config file",
		},
		{
			DestP:   &options.logLevel,
			Flag:    "log-level",
			Default: zapcore.InfoLevel.String(),
			Desc:    "supported log levels are debug, info, warn and error",
		},
		{
			DestP:   &options.logPath,
			Flag:    "log-path",
			Default: filepath.Join(homeOrAnyDir(), "upgrade.log"),
			Desc:    "optional: custom log file path",
		},
		{
			DestP:   &options.force,
			Flag:    "force",
			Default: false,
			Desc:    "skip the confirmation prompt",
			Short:   'f',
		},
	}

	cli.BindOptions(v, cmd, opts)
	// add sub commands
	cmd.AddCommand(v1DumpMetaCommand)
	cmd.AddCommand(v2DumpMetaCommand)
	return cmd
}

type influxDBv1 struct {
	meta *meta.Client
}

type influxDBv2 struct {
	log         *zap.Logger
	boltClient  *bolt.Client
	store       *bolt.KVStore
	kvStore     kv.SchemaStore
	tenantStore *tenant.Store
	ts          *tenant.Service
	dbrpSvc     influxdb.DBRPMappingServiceV2
	bucketSvc   influxdb.BucketService
	onboardSvc  influxdb.OnboardingService
	authSvc     *authv1.Service
	authSvcV2   influxdb.AuthorizationService
	meta        *meta.Client
}

func (i *influxDBv2) close() error {
	err := i.meta.Close()
	if err != nil {
		return err
	}
	err = i.boltClient.Close()
	if err != nil {
		return err
	}
	err = i.store.Close()
	if err != nil {
		return err
	}
	return nil
}

var fluxInitialized bool

func runUpgradeE(*cobra.Command, []string) error {
	// This command is executed multiple times by test code. Initialization can happen only once.
	if !fluxInitialized {
		fluxinit.FluxInit()
		fluxInitialized = true
	}

	var lvl zapcore.Level
	if err := lvl.Set(options.logLevel); err != nil {
		return errors.New("unknown log level; supported levels are debug, info, warn and error")
	}

	ctx := context.Background()
	config := zap.NewProductionConfig()
	config.Level = zap.NewAtomicLevelAt(lvl)
	config.OutputPaths = append(config.OutputPaths, options.logPath)
	config.ErrorOutputPaths = append(config.ErrorOutputPaths, options.logPath)
	log, err := config.Build()
	if err != nil {
		return err
	}

	err = validatePaths(&options.source, &options.target)
	if err != nil {
		return err
	}

	log.Info("Starting InfluxDB 1.x upgrade")

	var authEnabled bool
	if options.source.configFile != "" {
		log.Info("Upgrading config file", zap.String("file", options.source.configFile))
		v1Config, err := upgradeConfig(options.source.configFile, options.target, log)
		if err != nil {
			return err
		}
		options.source.metaDir = v1Config.Meta.Dir
		options.source.dataDir = v1Config.Data.Dir
		options.source.walDir = v1Config.Data.WALDir
		options.source.dbURL = v1Config.dbURL()
		authEnabled = v1Config.Http.AuthEnabled
	} else {
		log.Info("No InfluxDB 1.x config file specified, skipping its upgrade")
	}

	log.Info("Upgrade source paths", zap.String("meta", options.source.metaDir), zap.String("data", options.source.dataDir))
	log.Info("Upgrade target paths", zap.String("bolt", options.target.boltPath), zap.String("engine", options.target.enginePath))

	v1, err := newInfluxDBv1(&options.source)
	if err != nil {
		return err
	}

	v2, err := newInfluxDBv2(ctx, &options.target, log)
	if err != nil {
		return err
	}

	defer func() {
		if err := v2.close(); err != nil {
			log.Error("Failed to close 2.0 services.", zap.Error(err))
		}
	}()

	canOnboard, err := v2.onboardSvc.IsOnboarding(ctx)
	if err != nil {
		return err
	}

	if !canOnboard {
		return errors.New("InfluxDB has been already set up")
	}

	req, err := onboardingRequest()
	if err != nil {
		return err
	}
	or, err := setupAdmin(ctx, v2, req)
	if err != nil {
		return err
	}

	options.target.orgID = or.Org.ID
	options.target.userID = or.User.ID
	options.target.token = or.Auth.Token

	err = saveLocalConfig(&options.source, &options.target, log)
	if err != nil {
		return err
	}

	db2BucketIds, err := upgradeDatabases(ctx, v1, v2, &options.source, &options.target, or.Org.ID, log)
	if err != nil {
		//remove all files
		log.Info("Database upgrade error, removing data")
		if e := os.Remove(options.target.boltPath); e != nil {
			log.Error("Unable to remove bolt database.", zap.Error(e))
		}

		if e := os.RemoveAll(options.target.enginePath); e != nil {
			log.Error("Unable to remove time series data.", zap.Error(e))
		}
		return err
	}

	usersUpgraded, err := upgradeUsers(ctx, v1, v2, &options.target, db2BucketIds, log)
	if err != nil {
		return err
	}
	if usersUpgraded > 0 && !authEnabled {
		log.Warn(
			"V1 users were upgraded, but V1 auth was not enabled. Existing clients will fail authentication against V2 if using invalid credentials.",
		)
	}

	log.Info("Upgrade successfully completed. Start service now")

	return nil
}

// validatePaths ensures that all filesystem paths provided as input
// are usable by the upgrade command
func validatePaths(sourceOpts *optionsV1, targetOpts *optionsV2) error {
	fi, err := os.Stat(sourceOpts.dbDir)
	if err != nil {
		return fmt.Errorf("1.x DB dir '%s' does not exist", sourceOpts.dbDir)
	}
	if !fi.IsDir() {
		return fmt.Errorf("1.x DB dir '%s' is not a directory", sourceOpts.dbDir)
	}
	sourceOpts.populateDirs()

	metaDb := filepath.Join(sourceOpts.metaDir, "meta.db")
	_, err = os.Stat(metaDb)
	if err != nil {
		return fmt.Errorf("1.x meta.db '%s' does not exist", metaDb)
	}

	if sourceOpts.configFile != "" {
		_, err = os.Stat(sourceOpts.configFile)
		if err != nil {
			return fmt.Errorf("1.x config file '%s' does not exist", sourceOpts.configFile)
		}
		v2Config := translateV1ConfigPath(sourceOpts.configFile)
		if _, err := os.Stat(v2Config); err == nil {
			return fmt.Errorf("file present at target path for upgraded 2.x config file '%s'", v2Config)
		}
	}

	if _, err = os.Stat(targetOpts.boltPath); err == nil {
		return fmt.Errorf("file present at target path for upgraded 2.x bolt DB: '%s'", targetOpts.boltPath)
	}

	if fi, err = os.Stat(targetOpts.enginePath); err == nil {
		if !fi.IsDir() {
			return fmt.Errorf("upgraded 2.x engine path '%s' is not a directory", targetOpts.enginePath)
		}
		entries, err := ioutil.ReadDir(targetOpts.enginePath)
		if err != nil {
			return err
		}
		if len(entries) > 0 {
			return fmt.Errorf("upgraded 2.x engine directory '%s' must be empty", targetOpts.enginePath)
		}
	}

	if _, err = os.Stat(targetOpts.cliConfigsPath); err == nil {
		return fmt.Errorf("file present at target path for 2.x CLI configs '%s'", targetOpts.cliConfigsPath)
	}

	if _, err = os.Stat(targetOpts.cqPath); err == nil {
		return fmt.Errorf("file present at target path for exported continuous queries '%s'", targetOpts.cqPath)
	}

	return nil
}

func newInfluxDBv1(opts *optionsV1) (svc *influxDBv1, err error) {
	svc = &influxDBv1{}
	svc.meta, err = openV1Meta(opts.metaDir)
	if err != nil {
		return nil, fmt.Errorf("error opening 1.x meta.db: %w", err)
	}

	return svc, nil
}

func newInfluxDBv2(ctx context.Context, opts *optionsV2, log *zap.Logger) (svc *influxDBv2, err error) {
	reg := prom.NewRegistry(log.With(zap.String("service", "prom_registry")))

	svc = &influxDBv2{}
	svc.log = log

	// Create BoltDB store and K/V service
	svc.boltClient = bolt.NewClient(log.With(zap.String("service", "bolt")))
	svc.boltClient.Path = opts.boltPath
	if err := svc.boltClient.Open(ctx); err != nil {
		log.Error("Failed opening bolt", zap.Error(err))
		return nil, err
	}

	svc.store = bolt.NewKVStore(log.With(zap.String("service", "kvstore-bolt")), opts.boltPath)
	svc.store.WithDB(svc.boltClient.DB())
	svc.kvStore = svc.store

	// ensure migrator is run
	migrator, err := migration.NewMigrator(
		log.With(zap.String("service", "migrations")),
		svc.kvStore,
		all.Migrations[:]...,
	)
	if err != nil {
		log.Error("Failed to initialize kv migrator", zap.Error(err))
		return nil, err
	}

	// apply migrations to metadata store
	if err := migrator.Up(ctx); err != nil {
		log.Error("Failed to apply migrations", zap.Error(err))
		return nil, err
	}

	// Create Tenant service (orgs, buckets, )
	svc.tenantStore = tenant.NewStore(svc.kvStore)
	svc.ts = tenant.NewSystem(svc.tenantStore, log.With(zap.String("store", "new")), reg, metric.WithSuffix("new"))

	svc.meta = meta.NewClient(meta.NewConfig(), svc.kvStore)
	if err := svc.meta.Open(); err != nil {
		return nil, err
	}

	// DB/RP service
	svc.dbrpSvc = dbrp.NewService(ctx, svc.ts.BucketService, svc.kvStore)
	svc.bucketSvc = svc.ts.BucketService

	engine := storage.NewEngine(
		opts.enginePath,
		storage.NewConfig(),
		storage.WithMetaClient(svc.meta),
	)

	svc.ts.BucketService = storage.NewBucketService(log, svc.ts.BucketService, engine)

	authStoreV2, err := authorization.NewStore(svc.store)
	if err != nil {
		return nil, err
	}

	svc.authSvcV2 = authorization.NewService(authStoreV2, svc.ts)

	// on-boarding service (influx setup)
	svc.onboardSvc = tenant.NewOnboardService(svc.ts, svc.authSvcV2)

	// v1 auth service
	authStoreV1, err := authv1.NewStore(svc.kvStore)
	if err != nil {
		return nil, err
	}

	svc.authSvc = authv1.NewService(authStoreV1, svc.ts)

	return svc, nil
}

func openV1Meta(dir string) (*meta.Client, error) {
	cfg := meta.NewConfig()
	cfg.Dir = dir
	store := filestore.New(cfg.Dir, string(meta.BucketName), "meta.db")
	c := meta.NewClient(cfg, store)
	if err := c.Open(); err != nil {
		return nil, err
	}

	return c, nil
}

// influxDirV1 retrieves the influxdb directory.
func influxDirV1() (string, error) {
	var dir string
	// By default, store meta and data files in current users home directory
	u, err := user.Current()
	if err == nil {
		dir = u.HomeDir
	} else if home := os.Getenv("HOME"); home != "" {
		dir = home
	} else {
		wd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		dir = wd
	}
	dir = filepath.Join(dir, ".influxdb")

	return dir, nil
}

// influxConfigPathV1 returns default 1.x config file path or empty path if not found.
func influxConfigPathV1() string {
	if envVar := os.Getenv("INFLUXDB_CONFIG_PATH"); envVar != "" {
		return envVar
	}
	for _, path := range []string{
		os.ExpandEnv("${HOME}/.influxdb/influxdb.conf"),
		"/etc/influxdb/influxdb.conf",
	} {
		if _, err := os.Stat(path); err == nil {
			return path
		}
	}

	return ""
}

// homeOrAnyDir retrieves user's home directory, current working one or just none.
func homeOrAnyDir() string {
	var dir string
	u, err := user.Current()
	if err == nil {
		dir = u.HomeDir
	} else if home := os.Getenv("HOME"); home != "" {
		dir = home
	} else if home := os.Getenv("USERPROFILE"); home != "" {
		dir = home
	} else {
		wd, err := os.Getwd()
		if err != nil {
			dir = ""
		} else {
			dir = wd
		}
	}

	return dir
}
