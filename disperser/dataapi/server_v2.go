package dataapi

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/Layr-Labs/eigenda/core"
	corev2 "github.com/Layr-Labs/eigenda/core/v2"
	"github.com/Layr-Labs/eigenda/disperser/common/v2/blobstore"
	"github.com/Layr-Labs/eigenda/disperser/dataapi/docs"
	"github.com/Layr-Labs/eigensdk-go/logging"
	"github.com/gin-contrib/cors"
	"github.com/gin-contrib/logger"
	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"
	swaggerfiles "github.com/swaggo/files"
	ginswagger "github.com/swaggo/gin-swagger"
)

type (
	SignedBatch struct {
		BatchHeader *corev2.BatchHeader `json:"batch_header"`
		Attestation *corev2.Attestation `json:"attestation"`
	}

	BlobResponse struct {
		BlobHeader    *corev2.BlobHeader `json:"blob_header"`
		Status        string             `json:"status"`
		DispersedAt   uint64             `json:"dispersed_at"`
		BlobSizeBytes uint64             `json:"blob_size_bytes"`
	}

	BatchResponse struct {
		BatchHeaderHash       string                         `json:"batch_header_hash"`
		SignedBatch           *SignedBatch                   `json:"signed_batch"`
		BlobVerificationInfos []*corev2.BlobVerificationInfo `json:"blob_verification_infos"`
	}
)

type ServerInterface interface {
	Start() error
	Shutdown() error
}

type ServerV2 struct {
	serverMode   string
	socketAddr   string
	allowOrigins []string
	logger       logging.Logger

	blobMetadataStore *blobstore.BlobMetadataStore
	subgraphClient    SubgraphClient
	chainReader       core.Reader
	chainState        core.ChainState
	indexedChainState core.IndexedChainState
	promClient        PrometheusClient
	metrics           *Metrics

	operatorHandler *operatorHandler
}

func NewServerV2(
	config Config,
	blobMetadataStore *blobstore.BlobMetadataStore,
	promClient PrometheusClient,
	subgraphClient SubgraphClient,
	chainReader core.Reader,
	chainState core.ChainState,
	indexedChainState core.IndexedChainState,
	logger logging.Logger,
	metrics *Metrics,
) *ServerV2 {
	l := logger.With("component", "DataAPIServerV2")
	return &ServerV2{
		logger:            l,
		serverMode:        config.ServerMode,
		socketAddr:        config.SocketAddr,
		allowOrigins:      config.AllowOrigins,
		blobMetadataStore: blobMetadataStore,
		promClient:        promClient,
		subgraphClient:    subgraphClient,
		chainReader:       chainReader,
		chainState:        chainState,
		indexedChainState: indexedChainState,
		metrics:           metrics,
		operatorHandler:   newOperatorHandler(l, metrics, chainReader, chainState, indexedChainState, subgraphClient),
	}
}

func (s *ServerV2) Start() error {
	if s.serverMode == gin.ReleaseMode {
		// optimize performance and disable debug features.
		gin.SetMode(gin.ReleaseMode)
	}

	router := gin.New()
	basePath := "/api/v2"
	docs.SwaggerInfo.BasePath = basePath
	docs.SwaggerInfo.Host = os.Getenv("SWAGGER_HOST")
	v2 := router.Group(basePath)
	{
		blob := v2.Group("/blob")
		{
			blob.GET("/blob/feed", s.FetchBlobFeedHandler)
			blob.GET("/blob/:blob_key", s.FetchBlobHandler)
		}
		batch := v2.Group("/batch")
		{
			batch.GET("/batch/feed", s.FetchBatchFeedHandler)
			batch.GET("/batch/:batch_header_hash", s.FetchBatchHandler)
		}
		operators := v2.Group("/operators")
		{
			operators.GET("/non-signers", s.FetchNonSingers)
			operators.GET("/stake", s.FetchOperatorsStake)
			operators.GET("/nodeinfo", s.FetchOperatorsNodeInfo)
			operators.GET("/reachability", s.CheckOperatorsReachability)
		}
		metrics := v2.Group("/metrics")
		{
			metrics.GET("/overview", s.FetchMetricsOverviewHandler)
			metrics.GET("/throughput", s.FetchMetricsThroughputHandler)
		}
		swagger := v2.Group("/swagger")
		{
			swagger.GET("/*any", ginswagger.WrapHandler(swaggerfiles.Handler))
		}
	}

	router.GET("/", func(g *gin.Context) {
		g.JSON(http.StatusAccepted, gin.H{"status": "OK"})
	})

	router.Use(logger.SetLogger(
		logger.WithSkipPath([]string{"/"}),
	))

	config := cors.DefaultConfig()
	config.AllowOrigins = s.allowOrigins
	config.AllowCredentials = true
	config.AllowMethods = []string{"GET", "POST", "HEAD", "OPTIONS"}

	if s.serverMode != gin.ReleaseMode {
		config.AllowOrigins = []string{"*"}
	}
	router.Use(cors.New(config))

	srv := &http.Server{
		Addr:              s.socketAddr,
		Handler:           router,
		ReadTimeout:       5 * time.Second,
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      20 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	errChan := run(s.logger, srv)
	return <-errChan
}

func (s *ServerV2) Shutdown() error {
	return nil
}

func (s *ServerV2) FetchBlobFeedHandler(c *gin.Context) {
	errorResponse(c, errors.New("FetchBlobFeedHandler unimplemented"))
}

// FetchBlobHandler godoc
//
//	@Summary	Fetch blob metadata by blob key
//	@Tags		Feed
//	@Produce	json
//	@Param		blob_key	path		string	true	"Blob key in hex string"
//	@Success	200			{object}	BlobResponse
//	@Failure	400			{object}	ErrorResponse	"error: Bad request"
//	@Failure	404			{object}	ErrorResponse	"error: Not found"
//	@Failure	500			{object}	ErrorResponse	"error: Server error"
//	@Router		/blob/{blob_key} [get]
func (s *ServerV2) FetchBlobHandler(c *gin.Context) {
	start := time.Now()
	blobKey, err := corev2.HexToBlobKey(c.Param("blob_key"))
	if err != nil {
		s.metrics.IncrementInvalidArgRequestNum("FetchBlob")
		errorResponse(c, err)
		return
	}
	metadata, err := s.blobMetadataStore.GetBlobMetadata(c.Request.Context(), blobKey)
	if err != nil {
		s.metrics.IncrementFailedRequestNum("FetchBlob")
		errorResponse(c, err)
		return
	}
	response := &BlobResponse{
		BlobHeader:    metadata.BlobHeader,
		Status:        metadata.BlobStatus.String(),
		DispersedAt:   metadata.RequestedAt,
		BlobSizeBytes: metadata.BlobSize,
	}
	s.metrics.IncrementSuccessfulRequestNum("FetchBlob")
	s.metrics.ObserveLatency("FetchBlob", float64(time.Since(start).Milliseconds()))
	c.Writer.Header().Set(cacheControlParam, fmt.Sprintf("max-age=%d", maxFeedBlobAge))
	c.JSON(http.StatusOK, response)
}

func (s *ServerV2) FetchBatchFeedHandler(c *gin.Context) {
	errorResponse(c, errors.New("FetchBatchFeedHandler unimplemented"))
}

// FetchBatchHandler godoc
//
//	@Summary	Fetch batch by the batch header hash
//	@Tags		Feed
//	@Produce	json
//	@Param		batch_header_hash	path		string	true	"Batch header hash in hex string"
//	@Success	200					{object}	BlobResponse
//	@Failure	400					{object}	ErrorResponse	"error: Bad request"
//	@Failure	404					{object}	ErrorResponse	"error: Not found"
//	@Failure	500					{object}	ErrorResponse	"error: Server error"
//	@Router		/batch/{batch_header_hash} [get]
func (s *ServerV2) FetchBatchHandler(c *gin.Context) {
	start := time.Now()
	batchHeaderHashHex := c.Param("batch_header_hash")
	batchHeaderHash, err := ConvertHexadecimalToBytes([]byte(batchHeaderHashHex))
	if err != nil {
		s.metrics.IncrementInvalidArgRequestNum("FetchBatch")
		errorResponse(c, errors.New("invalid batch header hash"))
		return
	}
	batchHeader, attestation, err := s.blobMetadataStore.GetSignedBatch(c.Request.Context(), batchHeaderHash)
	if err != nil {
		s.metrics.IncrementFailedRequestNum("FetchBatch")
		errorResponse(c, err)
		return
	}
	// TODO: support fetch of blob verification info
	batchResponse := &BatchResponse{
		BatchHeaderHash: batchHeaderHashHex,
		SignedBatch: &SignedBatch{
			BatchHeader: batchHeader,
			Attestation: attestation,
		},
	}
	s.metrics.IncrementSuccessfulRequestNum("FetchBatch")
	s.metrics.ObserveLatency("FetchBatch", float64(time.Since(start).Milliseconds()))
	c.Writer.Header().Set(cacheControlParam, fmt.Sprintf("max-age=%d", maxFeedBlobAge))
	c.JSON(http.StatusOK, batchResponse)
}

// FetchOperatorsStake godoc
//
//	@Summary	Operator stake distribution query
//	@Tags		OperatorsStake
//	@Produce	json
//	@Param		operator_id	query		string	false	"Operator ID in hex string [default: all operators if unspecified]"
//	@Success	200			{object}	OperatorsStakeResponse
//	@Failure	400			{object}	ErrorResponse	"error: Bad request"
//	@Failure	404			{object}	ErrorResponse	"error: Not found"
//	@Failure	500			{object}	ErrorResponse	"error: Server error"
//	@Router		/operators/stake [get]
func (s *ServerV2) FetchOperatorsStake(c *gin.Context) {
	timer := prometheus.NewTimer(prometheus.ObserverFunc(func(f float64) {
		s.metrics.ObserveLatency("FetchOperatorsStake", f*1000) // make milliseconds
	}))
	defer timer.ObserveDuration()

	operatorId := c.DefaultQuery("operator_id", "")
	s.logger.Info("getting operators stake distribution", "operatorId", operatorId)

	operatorsStakeResponse, err := s.operatorHandler.getOperatorsStake(c.Request.Context(), operatorId)
	if err != nil {
		s.metrics.IncrementFailedRequestNum("FetchOperatorsStake")
		errorResponse(c, fmt.Errorf("failed to get operator stake - %s", err))
		return
	}

	s.metrics.IncrementSuccessfulRequestNum("FetchOperatorsStake")
	c.Writer.Header().Set(cacheControlParam, fmt.Sprintf("max-age=%d", maxOperatorsStakeAge))
	c.JSON(http.StatusOK, operatorsStakeResponse)
}

// FetchOperatorsNodeInfo godoc
//
//	@Summary	Active operator semver
//	@Tags		OperatorsNodeInfo
//	@Produce	json
//	@Success	200	{object}	SemverReportResponse
//	@Failure	500	{object}	ErrorResponse	"error: Server error"
//	@Router		/operators/nodeinfo [get]
func (s *ServerV2) FetchOperatorsNodeInfo(c *gin.Context) {
	timer := prometheus.NewTimer(prometheus.ObserverFunc(func(f float64) {
		s.metrics.ObserveLatency("FetchOperatorsNodeInfo", f*1000) // make milliseconds
	}))
	defer timer.ObserveDuration()

	report, err := s.operatorHandler.scanOperatorsHostInfo(c.Request.Context())
	if err != nil {
		s.logger.Error("failed to scan operators host info", "error", err)
		s.metrics.IncrementFailedRequestNum("FetchOperatorsNodeInfo")
		errorResponse(c, err)
	}
	c.Writer.Header().Set(cacheControlParam, fmt.Sprintf("max-age=%d", maxOperatorPortCheckAge))
	c.JSON(http.StatusOK, report)
}

// CheckOperatorsReachability godoc
//
//	@Summary	Operator node reachability check
//	@Tags		OperatorsReachability
//	@Produce	json
//	@Param		operator_id	query		string	false	"Operator ID in hex string [default: all operators if unspecified]"
//	@Success	200			{object}	OperatorPortCheckResponse
//	@Failure	400			{object}	ErrorResponse	"error: Bad request"
//	@Failure	404			{object}	ErrorResponse	"error: Not found"
//	@Failure	500			{object}	ErrorResponse	"error: Server error"
//	@Router		/operators/reachability [get]
func (s *ServerV2) CheckOperatorsReachability(c *gin.Context) {
	timer := prometheus.NewTimer(prometheus.ObserverFunc(func(f float64) {
		s.metrics.ObserveLatency("OperatorPortCheck", f*1000) // make milliseconds
	}))
	defer timer.ObserveDuration()

	operatorId := c.DefaultQuery("operator_id", "")
	s.logger.Info("checking operator ports", "operatorId", operatorId)
	portCheckResponse, err := s.operatorHandler.probeOperatorHosts(c.Request.Context(), operatorId)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			err = errNotFound
			s.logger.Warn("operator not found", "operatorId", operatorId)
			s.metrics.IncrementNotFoundRequestNum("OperatorPortCheck")
		} else {
			s.logger.Error("operator port check failed", "error", err)
			s.metrics.IncrementFailedRequestNum("OperatorPortCheck")
		}
		errorResponse(c, err)
		return
	}
	c.Writer.Header().Set(cacheControlParam, fmt.Sprintf("max-age=%d", maxOperatorPortCheckAge))
	c.JSON(http.StatusOK, portCheckResponse)
}

func (s *ServerV2) FetchNonSingers(c *gin.Context) {
	errorResponse(c, errors.New("FetchNonSingers unimplemented"))
}

func (s *ServerV2) FetchMetricsOverviewHandler(c *gin.Context) {
	errorResponse(c, errors.New("FetchMetricsOverviewHandler unimplemented"))
}

func (s *ServerV2) FetchMetricsThroughputHandler(c *gin.Context) {
	errorResponse(c, errors.New("FetchMetricsThroughputHandler unimplemented"))
}
