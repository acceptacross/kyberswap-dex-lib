package eulerswap

import (
	"context"
	"math/big"
	"strings"
	"time"

	"github.com/KyberNetwork/ethrpc"
	"github.com/KyberNetwork/logger"
	"github.com/ethereum/go-ethereum/common"
	"github.com/goccy/go-json"
	"github.com/holiman/uint256"

	"github.com/KyberNetwork/kyberswap-dex-lib/pkg/entity"
	uniswapv2 "github.com/KyberNetwork/kyberswap-dex-lib/pkg/liquidity-source/uniswap-v2"
	poollist "github.com/KyberNetwork/kyberswap-dex-lib/pkg/source/pool/list"
)

type PoolsListUpdater struct {
	config       *Config
	ethrpcClient *ethrpc.Client
}

var _ = poollist.RegisterFactoryCE(DexType, NewPoolsListUpdater)

func NewPoolsListUpdater(
	cfg *Config,
	ethrpcClient *ethrpc.Client,
) *PoolsListUpdater {
	return &PoolsListUpdater{
		config:       cfg,
		ethrpcClient: ethrpcClient,
	}
}

func (u *PoolsListUpdater) GetNewPools(ctx context.Context, metadataBytes []byte) ([]entity.Pool, []byte, error) {
	var (
		dexID     = u.config.DexID
		startTime = time.Now()
	)

	logger.WithFields(logger.Fields{"dex_id": dexID}).Info("Started getting new pools")

	offset, err := u.getOffset(metadataBytes)
	if err != nil {
		logger.
			WithFields(logger.Fields{"dex_id": dexID, "err": err}).
			Warn("getOffset failed")
	}

	allPoolsLength, err := u.getAllPoolsLength(ctx)
	if err != nil {
		logger.
			WithFields(logger.Fields{"dex_id": dexID}).
			Error("allPoolsLength failed")

		return nil, metadataBytes, err
	}

	if offset == allPoolsLength {
		return nil, metadataBytes, nil
	}

	if offset > allPoolsLength {
		logger.WithFields(logger.Fields{
			"dex_id": dexID,
			"offset": offset,
			"length": allPoolsLength,
		}).Info("Resetting offset to 0 due to factory uninstall pools")
		offset = 0
	}

	batchSize := u.getBatchSize(allPoolsLength, offset)

	poolAddresses, err := u.listPoolAddresses(ctx, offset, batchSize)
	if err != nil {
		logger.
			WithFields(logger.Fields{"dex_id": dexID, "err": err}).
			Error("listPoolAddresses failed")

		return nil, metadataBytes, err
	}

	pools, err := u.initPools(ctx, poolAddresses)
	if err != nil {
		logger.
			WithFields(logger.Fields{"dex_id": dexID, "err": err}).
			Error("initPools failed")

		return nil, metadataBytes, err
	}

	newMetadataBytes, err := u.newMetadata(offset + batchSize)
	if err != nil {
		logger.
			WithFields(logger.Fields{"dex_id": dexID, "err": err}).
			Error("newMetadata failed")

		return nil, metadataBytes, err
	}

	logger.
		WithFields(
			logger.Fields{
				"dex_id":      dexID,
				"pools_len":   len(pools),
				"duration_ms": time.Since(startTime).Milliseconds(),
			},
		).
		Info("Finished getting new pools")

	return pools, newMetadataBytes, nil
}

func (u *PoolsListUpdater) listPoolAddresses(ctx context.Context, offset, batchSize int) ([]common.Address, error) {
	result := []common.Address{}

	startIdx := big.NewInt(int64(offset))
	endIdx := big.NewInt(int64(offset + batchSize))

	req := u.ethrpcClient.NewRequest().SetContext(ctx)
	req.AddCall(&ethrpc.Call{
		ABI:    factoryABI,
		Target: u.config.FactoryAddress,
		Method: factoryMethodPoolsSlice,
		Params: []any{startIdx, endIdx},
	}, []any{&result})

	_, err := req.Aggregate()
	if err != nil {
		return nil, err
	}

	return result, nil
}

func (u *PoolsListUpdater) initPools(ctx context.Context, poolAddresses []common.Address) ([]entity.Pool, error) {
	tokensByPool, err := u.listPoolTokens(ctx, poolAddresses)
	if err != nil {
		return nil, err
	}

	pools := make([]entity.Pool, 0, len(poolAddresses))

	for i, poolAddress := range poolAddresses {
		staticPoolData, err := u.getPoolStaticData(ctx, poolAddress.Hex())
		if err != nil {
			return nil, err
		}

		extraBytes, err := json.Marshal(&staticPoolData)
		if err != nil {
			return nil, err
		}

		token0 := &entity.PoolToken{
			Address:   strings.ToLower(tokensByPool[i][0].Hex()),
			Swappable: true,
		}

		token1 := &entity.PoolToken{
			Address:   strings.ToLower(tokensByPool[i][1].Hex()),
			Swappable: true,
		}

		var newPool = entity.Pool{
			Address:     strings.ToLower(poolAddress.Hex()),
			Exchange:    u.config.DexID,
			Type:        DexType,
			Timestamp:   time.Now().Unix(),
			Reserves:    []string{"0", "0"},
			Tokens:      []*entity.PoolToken{token0, token1},
			StaticExtra: string(extraBytes),
		}

		pools = append(pools, newPool)
	}

	return pools, nil
}

func (u *PoolsListUpdater) listPoolTokens(ctx context.Context, poolAddresses []common.Address) ([][2]common.Address, error) {
	var poolTokens = make([][2]common.Address, len(poolAddresses))

	req := u.ethrpcClient.NewRequest().SetContext(ctx)

	for i, poolAddress := range poolAddresses {
		req.AddCall(&ethrpc.Call{
			ABI:    poolABI,
			Target: poolAddress.Hex(),
			Method: poolMethodGetAssets,
			Params: nil,
		}, []any{&poolTokens[i]})
	}

	if _, err := req.Aggregate(); err != nil {
		return nil, err
	}

	return poolTokens, nil
}

func (d *PoolsListUpdater) getPoolStaticData(
	ctx context.Context,
	poolAddress string,
) (StaticExtra, error) {
	var (
		params ParamsRPC
		evc    common.Address
	)

	req := d.ethrpcClient.NewRequest().SetContext(ctx)
	req.AddCall(&ethrpc.Call{
		ABI:    poolABI,
		Target: poolAddress,
		Method: poolMethodGetParams,
		Params: nil,
	}, []any{&params})

	req.AddCall(&ethrpc.Call{
		ABI:    poolABI,
		Target: poolAddress,
		Method: poolMethodEVC,
		Params: nil,
	}, []any{&evc})

	_, err := req.Aggregate()
	if err != nil {
		return StaticExtra{}, err
	}

	poolData := StaticExtra{
		Vault0:               params.Data.Vault0.Hex(),
		Vault1:               params.Data.Vault1.Hex(),
		EulerAccount:         params.Data.EulerAccount.Hex(),
		EquilibriumReserve0:  uint256.MustFromBig(params.Data.EquilibriumReserve0),
		EquilibriumReserve1:  uint256.MustFromBig(params.Data.EquilibriumReserve1),
		PriceX:               uint256.MustFromBig(params.Data.PriceX),
		PriceY:               uint256.MustFromBig(params.Data.PriceY),
		Fee:                  uint256.MustFromBig(params.Data.Fee),
		ProtocolFee:          uint256.MustFromBig(params.Data.ProtocolFee),
		ConcentrationX:       uint256.MustFromBig(params.Data.ConcentrationX),
		ConcentrationY:       uint256.MustFromBig(params.Data.ConcentrationY),
		ProtocolFeeRecipient: params.Data.ProtocolFeeRecipient,
		EVC:                  evc.Hex(),
	}

	return poolData, nil
}

func (u *PoolsListUpdater) getAllPoolsLength(ctx context.Context) (int, error) {
	var allPoolsLength *big.Int

	req := u.ethrpcClient.NewRequest().SetContext(ctx)

	req.AddCall(&ethrpc.Call{
		ABI:    factoryABI,
		Target: u.config.FactoryAddress,
		Method: factoryMethodPoolsLength,
		Params: nil,
	}, []any{&allPoolsLength})

	if _, err := req.Call(); err != nil {
		return 0, err
	}

	return int(allPoolsLength.Int64()), nil
}

func (u *PoolsListUpdater) newMetadata(newOffset int) ([]byte, error) {
	metadata := uniswapv2.PoolsListUpdaterMetadata{
		Offset: newOffset,
	}

	metadataBytes, err := json.Marshal(metadata)
	if err != nil {
		return nil, err
	}

	return metadataBytes, nil
}

func (u *PoolsListUpdater) getOffset(metadataBytes []byte) (int, error) {
	if len(metadataBytes) == 0 {
		return 0, nil
	}

	var metadata uniswapv2.PoolsListUpdaterMetadata
	if err := json.Unmarshal(metadataBytes, &metadata); err != nil {
		return 0, err
	}

	return metadata.Offset, nil
}

func (u *PoolsListUpdater) getBatchSize(length int, offset int) int {
	if offset >= length {
		return 0
	}

	if offset+batchSize >= length {
		return length - offset
	}

	return batchSize
}
