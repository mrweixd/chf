// Copyright 2013-2015 go-diameter authors.  All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Diameter server example. This is by no means a complete server.
//
// If you'd like to test diameter over SSL, generate SSL certificates:
//   go run $GOROOT/src/crypto/tls/generate_cert.go --host localhost
//
// And start the server with `-cert_file cert.pem -key_file key.pem`.
//
// By default this server runs in a single OS thread. If you want to
// make it run on more, set the GOMAXPROCS=n environment variable.
// See Go's FAQ for details: http://golang.org/doc/faq#Why_no_multi_CPU

package rf

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"math"
	_ "net/http/pprof"
	"strconv"
	"sync"
	"time"

	"github.com/fiorix/go-diameter/diam"
	"github.com/fiorix/go-diameter/diam/datatype"
	"github.com/fiorix/go-diameter/diam/dict"
	"github.com/fiorix/go-diameter/diam/sm"
	"go.mongodb.org/mongo-driver/bson"

	charging_datatype "github.com/free5gc/chf/ccs_diameter/datatype"
	charging_dict "github.com/free5gc/chf/ccs_diameter/dict"
	"github.com/free5gc/chf/internal/logger"
	"github.com/free5gc/chf/internal/tariff"
	"github.com/free5gc/chf/pkg/factory"
	"github.com/free5gc/util/mongoapi"
)

const chargingDatasColl = "policyData.ues.chargingData"

func OpenServer(ctx context.Context, wg *sync.WaitGroup) {
	// Load our custom dictionary on top of the default one, which
	// always have the Base Protocol (RFC6733) and Credit Control
	// Application (RFC4006).
	mongodb := factory.ChfConfig.Configuration.Mongodb
	// Connect to MongoDB
	if err := mongoapi.SetMongoDB(mongodb.Name, mongodb.Url); err != nil {
		logger.InitLog.Errorf("InitpcfContext err: %+v", err)
		return
	}

	err := dict.Default.Load(bytes.NewReader([]byte(charging_dict.RateDictionary)))
	if err != nil {
		logger.RatingLog.Error(err)
	}
	settings := &sm.Settings{
		OriginHost:       datatype.DiameterIdentity("server"),
		OriginRealm:      datatype.DiameterIdentity("go-diameter"),
		VendorID:         13,
		ProductName:      "go-diameter",
		FirmwareRevision: 1,
	}

	// Create the state machine (mux) and set its message handlers.
	mux := sm.New(settings)
	mux.Handle("SUR", handleSUR())
	mux.HandleFunc("ALL", handleALL) // Catch all.

	// Print error reports.
	go printErrors(mux.ErrorReports())
	go func() {
		defer func() {
			logger.CgfLog.Infoln("Rating Function server stopped")
			wg.Done()
		}()
		<-ctx.Done()
	}()
	rfDiameter := factory.ChfConfig.Configuration.RfDiameter
	addr := rfDiameter.HostIPv4 + ":" + strconv.Itoa(rfDiameter.Port)
	go func() {
		errListen := diam.ListenAndServeTLS(addr, rfDiameter.Tls.Pem, rfDiameter.Tls.Key, mux, nil)
		if err != nil {
			log.Fatal(errListen)
		}
	}()
}

func printErrors(ec <-chan *diam.ErrorReport) {
	for err := range ec {
		logger.RatingLog.Errorf("Diam Error Report: %v", err)
	}
}

// Commenting the unused function
// func listen(addr, cert, key string, handler diam.Handler) error {
// 	// Start listening for connections.
// 	if len(cert) > 0 && len(key) > 0 {
// 		logger.RatingLog.Infof("Starting secure diameter server on %s", addr)
// 		return diam.ListenAndServeTLS(addr, cert, key, handler, nil)
// 	}

// 	logger.RatingLog.Infof("Starting diameter server on %s", addr)
// 	return diam.ListenAndServe(addr, handler, nil)
// }

func buildTariff(raw string) (*charging_datatype.MonetaryTariff, error) {
	digits, exponent, err := tariff.Decimal(raw)
	if err != nil {
		return nil, err
	}
	unitCost := &charging_datatype.UnitCost{
		ValueDigits: datatype.Integer64(digits), Exponent: datatype.Integer32(exponent),
	}
	return &charging_datatype.MonetaryTariff{
		CurrencyCode: datatype.Unsigned32(901),
		ScaleFactor: &charging_datatype.ScaleFactor{
			ValueDigits: datatype.Integer64(0),
			Exponent:    datatype.Integer32(0),
		},
		RateElement: &charging_datatype.RateElement{
			CCUnitType: charging_datatype.MONEY,
			UnitCost:   unitCost,
		},
	}, nil
}

func handleSUR() diam.HandlerFunc {
	return func(c diam.Conn, m *diam.Message) {
		var sur charging_datatype.ServiceUsageRequest

		var subscriberId string

		if err := m.Unmarshal(&sur); err != nil {
			logger.RatingLog.Errorf("Failed to parse message from %s: %s\n%s",
				c.RemoteAddr(), err, m)
			return
		}

		if sur.ServiceRating == nil || sur.SubscriptionId == nil {
			ratingFailure(m, c)
			return
		}
		sr := sur.ServiceRating
		rg := uint32(sr.ServiceIdentifier)

		switch sur.SubscriptionId.SubscriptionIdType {
		case charging_datatype.END_USER_IMSI:
			subscriberId = "imsi-" + string(sur.SubscriptionId.SubscriptionIdData)
		}
		// // Retrieve tarrif information from database

		filter := bson.M{"ueId": subscriberId, "ratingGroup": rg}
		chargingInterface, err := mongoapi.RestfulAPIGetOne(chargingDatasColl, filter)
		if err != nil || chargingInterface == nil {
			logger.RatingLog.Error("No usable charging data")
			ratingFailure(m, c)
			return
		}
		raw, ok := chargingInterface["unitCost"].(string)
		if !ok {
			ratingFailure(m, c)
			return
		}
		rated, err := rateUsage(raw, sr)
		if err != nil {
			logger.RatingLog.Error(err)
			ratingFailure(m, c)
			return
		}
		sua := charging_datatype.ServiceUsageResponse{
			SessionId: sur.SessionId, EventTimestamp: datatype.Time(time.Now()), ServiceRating: rated,
		}
		a, err := sua.SuccessAnswer(m)
		if err != nil {
			logger.RatingLog.Errorf("Marshal SUA Err: %+v:", err)
			ratingFailure(m, c)
			return
		}

		_, err = a.WriteTo(c)
		if err != nil {
			logger.RatingLog.Errorf("Failed to write message to %s: %s\n%s\n",
				c.RemoteAddr(), err, a)
			return
		}
	}
}

func handleALL(c diam.Conn, m *diam.Message) {
	logger.RatingLog.Warnf("Received unexpected message from %s:\n%s", c.RemoteAddr(), m)
}

func ratingFailure(m *diam.Message, c diam.Conn) {
	if _, err := m.Answer(diam.UnableToComply).WriteTo(c); err != nil {
		logger.RatingLog.Error(err)
	}
}

func rateUsage(raw string, sr *charging_datatype.ServiceRating) (*charging_datatype.ServiceRating, error) {
	t, err := buildTariff(raw)
	if err != nil {
		return nil, err
	}
	cost := t.RateElement.UnitCost
	p, err := tariff.New(int64(cost.ValueDigits), int32(cost.Exponent))
	if err != nil {
		return nil, err
	}
	result := &charging_datatype.ServiceRating{MonetaryTariff: t}
	volume := uint64(sr.ConsumedUnits)
	switch sr.RequestSubType {
	case charging_datatype.REQ_SUBTYPE_DEBIT:
	case charging_datatype.REQ_SUBTYPE_RESERVE:
		limit := uint64(math.MaxUint32)
		if sr.RequestedUnits != 0 {
			limit = uint64(sr.RequestedUnits)
		}
		volume = p.Affordable(uint64(sr.MonetaryQuota), limit)
		result.AllowedUnits = datatype.Unsigned32(volume)
	default:
		return nil, fmt.Errorf("unsupported rating request subtype")
	}
	amount, err := p.Charge(volume)
	if err != nil || amount > math.MaxUint32 {
		return nil, fmt.Errorf("price exceeds Diameter range")
	}
	wirePrice := datatype.Unsigned32(amount)
	result.Price = &wirePrice
	return result, nil
}
