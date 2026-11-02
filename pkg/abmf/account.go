package abmf

import (
	"context"
	"crypto/sha256"
	"fmt"
	"math"
	"strconv"

	"github.com/fiorix/go-diameter/diam"
	"github.com/fiorix/go-diameter/diam/datatype"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	cd "github.com/free5gc/chf/ccs_diameter/datatype"
)

// The most recent result for a session also serves as its replay watermark.
type receipt struct {
	Number  int64
	Action  int32
	Kind    int32
	Amount  int64
	After   int64
	Granted int64
	Final   bool
}

func account(ctx context.Context, coll *mongo.Collection, r *cd.AccountDebitRequest) (*cd.AccountDebitResponse, error) {
	m := r.MultipleServicesCreditControl
	if r.SessionId == "" || m == nil || r.SubscriptionId == nil ||
		r.SubscriptionId.SubscriptionIdType != cd.END_USER_IMSI {
		return nil, fmt.Errorf("incomplete account request")
	}
	var amount uint64
	switch {
	case r.RequestedAction == cd.DIRECT_DEBITING && r.CcRequestType == cd.TERMINATION_REQUEST && m.UsedServiceUnit != nil:
		amount = uint64(m.UsedServiceUnit.CCTotalOctets)
	case (r.RequestedAction == cd.REFUND_ACCOUNT || (r.RequestedAction == cd.DIRECT_DEBITING &&
		(r.CcRequestType == cd.INITIAL_REQUEST || r.CcRequestType == cd.UPDATE_REQUEST))) && m.RequestedServiceUnit != nil:
		amount = uint64(m.RequestedServiceUnit.CCTotalOctets)
	default:
		return nil, fmt.Errorf("unsupported account request")
	}
	if amount > math.MaxInt64 {
		return nil, fmt.Errorf("amount exceeds account range")
	}
	want := receipt{
		Number: int64(r.CcRequestNumber), Action: int32(r.RequestedAction),
		Kind: int32(r.CcRequestType), Amount: int64(amount),
	}
	key := fmt.Sprintf("%x", sha256.Sum256([]byte(r.SessionId)))
	history := receiptHistory(coll)
	id := bson.M{"ueId": "imsi-" + string(r.SubscriptionId.SubscriptionIdData), "ratingGroup": int64(m.RatingGroup)}
	lookup := options.FindOne().SetProjection(bson.M{"quota": 1, "chfLatest": 1, "chfReceipts": 1}).
		SetCollation(&options.Collation{Locale: "en_US", Strength: 2})
	for {
		var doc struct {
			ID       any `bson:"_id"`
			Quota    string
			Latest   *sessionReceipt    `bson:"chfLatest"`
			Receipts map[string]receipt `bson:"chfReceipts"`
		}
		err := coll.FindOne(ctx, id, lookup).Decode(&doc)
		if err != nil {
			return nil, err
		}
		if len(doc.Receipts) > 0 {
			if err = migrateReceipts(ctx, coll, history, doc.ID, doc.Receipts); err != nil {
				return nil, err
			}
			continue
		}
		var prev receipt
		exists := doc.Latest != nil && doc.Latest.Session == key
		if exists {
			prev = doc.Latest.Result
		} else {
			var saved struct{ Result receipt }
			err = history.FindOne(ctx, receiptID(doc.ID, key)).Decode(&saved)
			switch err {
			case nil:
				prev, exists = saved.Result, true
			case mongo.ErrNoDocuments:
			default:
				return nil, err
			}
		}

		if exists && want.Number <= prev.Number {
			if want.Number != prev.Number || want.Action != prev.Action || want.Kind != prev.Kind || want.Amount != prev.Amount {
				return nil, fmt.Errorf("stale or conflicting accounting request")
			}
			return prev.answer(r), nil
		}
		q, err := strconv.ParseInt(doc.Quota, 10, 64)
		if err != nil {
			return nil, err
		}
		next := want
		switch {
		case r.RequestedAction == cd.REFUND_ACCOUNT:
			if q > math.MaxInt64-want.Amount {
				return nil, fmt.Errorf("refund overflow")
			}
			next.After = q + want.Amount
		case r.CcRequestType == cd.TERMINATION_REQUEST:
			if q < math.MinInt64+want.Amount {
				return nil, fmt.Errorf("debit underflow")
			}
			next.After = q - want.Amount
		default:
			next.Granted = min(want.Amount, max(q, 0))
			next.Final = next.Granted < want.Amount
			next.After = q - next.Granted
		}
		// The balance and receipt become visible together. If another writer won,
		// reread both before computing or returning a result.
		match := bson.M{"_id": doc.ID, "quota": doc.Quota}
		if doc.Latest == nil {
			match["chfLatest"] = bson.M{"$exists": false}
		} else {
			match["chfLatest.session"] = doc.Latest.Session
			match["chfLatest.result.number"] = doc.Latest.Result.Number
			if doc.Latest.Session != key {
				// Preserve the old watermark before evicting it. A crash between these
				// writes leaves the same receipt in both locations and is safe to retry.
				_, err = history.BulkWrite(ctx, []mongo.WriteModel{archiveReceipt(doc.ID, *doc.Latest)})
				if err != nil {
					return nil, err
				}
			}
		}
		updates := bson.M{"chfLatest": sessionReceipt{Session: key, Result: next}, "quota": strconv.FormatInt(next.After, 10)}
		res, err := coll.UpdateOne(ctx, match, bson.M{"$set": updates})
		if err != nil {
			return nil, err
		}
		if res.MatchedCount == 1 {
			return next.answer(r), nil
		}
	}
}

func (s receipt) answer(r *cd.AccountDebitRequest) *cd.AccountDebitResponse {
	a := &cd.AccountDebitResponse{
		SessionId: r.SessionId, ResultCode: diam.Success, OriginHost: r.DestinationHost,
		OriginRealm: r.DestinationRealm, CcRequestType: r.CcRequestType, CcRequestNumber: r.CcRequestNumber,
		EventTimestamp:   r.EventTimestamp,
		RemainingBalance: &cd.RemainingBalance{UnitValue: &cd.UnitValue{ValueDigits: datatype.Integer64(s.After)}},
	}
	if r.RequestedAction == cd.DIRECT_DEBITING && r.CcRequestType != cd.TERMINATION_REQUEST {
		a.MultipleServicesCreditControl = &cd.MultipleServicesCreditControl{
			RatingGroup:        r.MultipleServicesCreditControl.RatingGroup,
			GrantedServiceUnit: &cd.GrantedServiceUnit{CCTotalOctets: datatype.Unsigned64(s.Granted)},
		}
		if s.Final {
			a.MultipleServicesCreditControl.FinalUnitIndication = &cd.FinalUnitIndication{FinalUnitAction: cd.TERMINATE}
		}
	}
	return a
}
