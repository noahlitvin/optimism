package batchsubmitter

import (
	"context"
	"math/big"
	"sync"
	"time"

	"github.com/ethereum-optimism/optimism/go/batch-submitter/metrics"
	"github.com/ethereum-optimism/optimism/go/batch-submitter/txmgr"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/log"
)

var (
	// weiToEth is the conversion rate from wei to ether.
	weiToEth = new(big.Float).SetFloat64(1e-18)
)

// Driver is an interface for creating and submitting batch transactions for a
// specific contract.
type Driver interface {
	// Name is an identifier used to prefix logs for a particular service.
	Name() string

	// WalletAddr is the wallet address used to pay for batch transaction
	// fees.
	WalletAddr() common.Address

	// Metrics returns the subservice telemetry object.
	Metrics() *metrics.Metrics

	// GetBatchBlockRange returns the start and end L2 block heights that
	// need to be processed. Note that the end value is *exclusive*,
	// therefore if the returned values are identical nothing needs to be
	// processed.
	GetBatchBlockRange(ctx context.Context) (*big.Int, *big.Int, error)

	// SubmitBatchTx transforms the L2 blocks between start and end into a
	// batch transaction using the given nonce and gasPrice. The final
	// transaction is published and returned to the call.
	SubmitBatchTx(
		ctx context.Context,
		start, end, nonce, gasPrice *big.Int,
	) (*types.Transaction, error)
}

type ServiceConfig struct {
	Context         context.Context
	Driver          Driver
	PollInterval    time.Duration
	L1Client        *ethclient.Client
	TxManagerConfig txmgr.Config
}

type Service struct {
	cfg    ServiceConfig
	ctx    context.Context
	cancel func()

	txMgr   txmgr.TxManager
	metrics *metrics.Metrics

	wg sync.WaitGroup
}

func NewService(cfg ServiceConfig) *Service {
	ctx, cancel := context.WithCancel(cfg.Context)

	txMgr := txmgr.NewSimpleTxManager(
		cfg.Driver.Name(), cfg.TxManagerConfig, cfg.L1Client,
	)

	return &Service{
		cfg:     cfg,
		ctx:     ctx,
		cancel:  cancel,
		txMgr:   txMgr,
		metrics: cfg.Driver.Metrics(),
	}
}

func (s *Service) Start() error {
	s.wg.Add(1)
	go s.eventLoop()
	return nil
}

func (s *Service) Stop() error {
	s.cancel()
	s.wg.Wait()
	return nil
}

func (s *Service) eventLoop() {
	defer s.wg.Done()

	name := s.cfg.Driver.Name()

	for {
		select {
		case <-time.After(s.cfg.PollInterval):
			// Record the submitter's current ETH balance. This is done first in
			// case any of the remaining steps fail, we can at least have an
			// accurate view of the submitter's balance.
			balance, err := s.cfg.L1Client.BalanceAt(
				s.ctx, s.cfg.Driver.WalletAddr(), nil,
			)
			if err != nil {
				log.Error(name+" unable to get current balance", "err", err)
				continue
			}
			s.metrics.ETHBalance.Set(weiToEth64(balance))

			// Determine the range of L2 blocks that the batch submitter has not
			// processed, and needs to take action on.
			log.Info(name + " fetching current block range")
			start, end, err := s.cfg.Driver.GetBatchBlockRange(s.ctx)
			if err != nil {
				log.Error(name+" unable to get block range", "err", err)
				continue
			}

			// No new updates.
			if start.Cmp(end) == 0 {
				log.Info(name+" no updates", "start", start, "end", end)
				continue
			}
			log.Info(name+" block range", "start", start, "end", end)

			// Query for the submitter's current nonce.
			nonce64, err := s.cfg.L1Client.NonceAt(
				s.ctx, s.cfg.Driver.WalletAddr(), nil,
			)
			if err != nil {
				log.Error(name+" unable to get current nonce",
					"err", err)
				continue
			}
			nonce := new(big.Int).SetUint64(nonce64)

			// Construct the transaction submission clousure that will attempt
			// to send the next transaction at the given nonce and gas price.
			sendTx := func(
				ctx context.Context,
				gasPrice *big.Int,
			) (*types.Transaction, error) {
				log.Info(name+" attempting batch tx", "start", start,
					"end", end, "nonce", nonce,
					"gasPrice", gasPrice)

				tx, err := s.cfg.Driver.SubmitBatchTx(
					ctx, start, end, nonce, gasPrice,
				)
				if err != nil {
					return nil, err
				}

				log.Info(
					name+" submitted batch tx",
					"start", start,
					"end", end,
					"nonce", nonce,
					"tx_hash", tx.Hash(),
					"gasPrice", gasPrice,
				)

				s.metrics.BatchSizeInBytes.Observe(float64(tx.Size()))

				return tx, nil
			}

			// Wait until one of our submitted transactions confirms. If no
			// receipt is received it's likely our gas price was too low.
			batchConfirmationStart := time.Now()
			receipt, err := s.txMgr.Send(s.ctx, sendTx)
			if err != nil {
				log.Error(name+" unable to publish batch tx",
					"err", err)
				s.metrics.FailedSubmissions.Inc()
				continue
			}

			// The transaction was successfully submitted.
			log.Info(name+" batch tx successfully published",
				"tx_hash", receipt.TxHash)
			batchConfirmationTime := time.Since(batchConfirmationStart) /
				time.Millisecond
			s.metrics.BatchConfirmationTime.Set(float64(batchConfirmationTime))
			s.metrics.BatchesSubmitted.Inc()
			s.metrics.SubmissionGasUsed.Set(float64(receipt.GasUsed))
			s.metrics.SubmissionTimestamp.Set(float64(time.Now().UnixNano() / 1e6))

		case err := <-s.ctx.Done():
			log.Error(name+" service shutting down", "err", err)
			return
		}
	}
}

func weiToEth64(wei *big.Int) float64 {
	eth := new(big.Float).SetInt(wei)
	eth.Mul(eth, weiToEth)
	eth64, _ := eth.Float64()
	return eth64
}
