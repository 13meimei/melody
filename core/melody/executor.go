package melody

import (
	"context"
	"io"
	"melody/cmd"
	"melody/config"
	"melody/logging"
	bloomfilter "melody/middleware/melody-bloomfilter"
	gelf "melody/middleware/melody-gelf"
	gologging "melody/middleware/melody-gologging"
	influxdb "melody/middleware/melody-influxdb"
	jose "melody/middleware/melody-jose"
	logstash "melody/middleware/melody-logstash"
	metrics "melody/middleware/melody-metrics/gin"
	melodyrouter "melody/router"
	router "melody/router/gin"
	server "melody/transport/http/server/plugin"
	"os"

	"github.com/gin-gonic/gin"
)

//NewExecutor return an new executor
func NewExecutor(ctx context.Context) cmd.Executor {
	return func(cfg config.ServiceConfig) {
		// 确定以及初始化 log有哪些输出
		var writers []io.Writer
		// 检察是否使用Gelf
		gelfWriter, err := gelf.NewWriter(cfg.ExtraConfig)
		if err == nil {
			writers = append(writers, GelfWriter{gelfWriter})
			gologging.UpdateFormatSelector(func(w io.Writer) string {
				switch w.(type) {
				case GelfWriter:
					return "%{message}"
				default:
					return gologging.DefaultPattern
				}
			})
		}
		// 初始化Logger

		// 是否启用logstash
		// Logstash 是开源的服务器端数据处理管道，能够同时从多个来源采集数据，转换数据，然后将数据发送到您最喜欢的“存储库”中。
		// 所以没有logstash就没有下面其他logger
		logger, enableLogstashError := logstash.NewLogger(cfg.ExtraConfig, writers...)

		if enableLogstashError != nil {
			// 是否使用gologging
			var enableGologgingError error
			logger, enableGologgingError = gologging.NewLogger(cfg.ExtraConfig, writers...)

			if enableGologgingError != nil {
				// 默认使用基础Log  Level:Debug, Output:stdout, Prefix: ""
				logger, err = logging.NewLogger("DEBUG", os.Stdout, "")
				if err != nil {
					return
				}
				logger.Error("unable to create gologging logger")
			} else {
				logger.Debug("use gologging as logger")
			}
		} else {
			logger.Debug("use logstash as logger")
		}

		if cfg.Plugin != nil {
			LoadPlugins(cfg.Plugin.Folder, cfg.Plugin.Pattern, logger)
		}

		// 注册etcd, dns srv,并返回func to register consul

		reg := RegisterSubscriberFactories(ctx, cfg, logger)
		// 创建Metrics监控
		metricsController := metrics.New(ctx, cfg.ExtraConfig, logger)
		// 集成influxdb （单独使用，为melody-data提供数据）
		if err := influxdb.Register(ctx, &cfg, metricsController, logger); err != nil {
			logger.Warning(err)
		}
		// 集成bloomFilter
		rejecter, err := bloomfilter.Register(ctx, "melody-bf", cfg, logger, reg)
		if err != nil {
			logger.Warning("bloomFilter:", err.Error())
		}

		// 集成JWT，注册RejecterFactory
		tokenRejecterFactory := jose.ChainedRejecterFactory([]jose.RejecterFactory{
			jose.RejecterFactoryFunc(func(_ logging.Logger, _ *config.EndpointConfig) jose.Rejecter {
				return jose.RejecterFunc(rejecter.RejectToken)
			}),
		})

		// Set up melody Router
		routerFactory := router.NewFactory(router.Config{
			Engine:         NewEngine(cfg, logger, gelfWriter),
			ProxyFactory:   NewProxyFactory(logger, NewBackendFactoryWithContext(ctx, logger, metricsController), metricsController),
			HandlerFactory: NewHandlerFactory(logger, tokenRejecterFactory, metricsController),
			MiddleWares:    []gin.HandlerFunc{},
			Logger:         logger,
			RunServer:      router.RunServerFunc(server.New(logger, melodyrouter.DefaultRunServer)),
		})

		logger.Info("melody server listening on port:", cfg.Port, "🎁")

		routerFactory.NewWithContext(ctx).Run(cfg)

	}
}

// GelfWriter 封装了io.Writer，作为gelf writer
type GelfWriter struct {
	io.Writer
}
