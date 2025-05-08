package config

import (
	"time"

	"github.com/spf13/viper"
)

type Config struct {
    Cluster struct {
        Peers []string `mapstructure:"peers"`
        Self  string   `mapstructure:"self"`
    } `mapstructure:"cluster"`

    Erasure struct {
        Data  int `mapstructure:"data"`
        Total int `mapstructure:"total"`
    } `mapstructure:"erasure"`

    Object struct {
        TTL time.Duration `mapstructure:"ttl"`
    } `mapstructure:"object"`

    Storage struct {
        Datadir string `mapstructure:"datadir"`
        DB      string `mapstructure:"db"`
    } `mapstructure:"storage"`

    Server struct {
        GRPCPort    int `mapstructure:"grpc_port"`
        MetricsPort int `mapstructure:"metrics_port"`
    } `mapstructure:"server"`
}

func Load(path string) (*Config, error) {
    v := viper.New()

    // ➊ YAML file (optional)
    if path != "" {
        v.SetConfigFile(path)
        if err := v.ReadInConfig(); err != nil {
            return nil, err
        }
    }

    // ➋ ENV overrides — e.g. AVID_ERASURE_DATA=4
    v.SetEnvPrefix("AVID")
    v.AutomaticEnv()

    // ➌ Hard defaults (match old behaviour)
    v.SetDefault("cluster.peers", []string{})
    v.SetDefault("erasure.data", 3)
    v.SetDefault("erasure.total", 5)
    v.SetDefault("object.ttl", "24h")
    v.SetDefault("storage.datadir", "data")
    v.SetDefault("storage.db", "store.db")
    v.SetDefault("server.grpc_port", 50051)
    v.SetDefault("server.metrics_port", 9102)

    var cfg Config
    if err := v.Unmarshal(&cfg); err != nil {
        return nil, err
    }
    return &cfg, nil
}