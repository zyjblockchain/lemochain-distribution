// Code generated by github.com/fjl/gencodec. DO NOT EDIT.

package node

import (
	"encoding/json"
	"errors"

	"github.com/LemoFoundationLtd/lemochain-core/chain/types"
	"github.com/LemoFoundationLtd/lemochain-core/common/hexutil"
)

var _ = (*AssetEquityBatchRspMarshaling)(nil)

// MarshalJSON marshals as JSON.
func (a AssetEquityBatchRsp) MarshalJSON() ([]byte, error) {
	type AssetEquityBatchRsp struct {
		Equities []*types.AssetEquity `json:"equities" gencodec:"required"`
		Total    hexutil.Uint32       `json:"total" gencodec:"required"`
	}
	var enc AssetEquityBatchRsp
	enc.Equities = a.Equities
	enc.Total = hexutil.Uint32(a.Total)
	return json.Marshal(&enc)
}

// UnmarshalJSON unmarshals from JSON.
func (a *AssetEquityBatchRsp) UnmarshalJSON(input []byte) error {
	type AssetEquityBatchRsp struct {
		Equities []*types.AssetEquity `json:"equities" gencodec:"required"`
		Total    *hexutil.Uint32      `json:"total" gencodec:"required"`
	}
	var dec AssetEquityBatchRsp
	if err := json.Unmarshal(input, &dec); err != nil {
		return err
	}
	if dec.Equities == nil {
		return errors.New("missing required field 'equities' for AssetEquityBatchRsp")
	}
	a.Equities = dec.Equities
	if dec.Total == nil {
		return errors.New("missing required field 'total' for AssetEquityBatchRsp")
	}
	a.Total = uint32(*dec.Total)
	return nil
}
