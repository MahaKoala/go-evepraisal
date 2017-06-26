package esi

import (
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/evepraisal/go-evepraisal"
	"github.com/sethgrid/pester"
)

type MarketOrder struct {
	ID            int64   `json:"order_id"`
	Type          int64   `json:"type_id"`
	StationID     int64   `json:"location_id"`
	Volume        int64   `json:"volume_remain"`
	MinVolume     int64   `json:"min_volume"`
	Price         float64 `json:"price"`
	Buy           bool    `json:"is_buy_order"`
	Duration      int64   `json:"duration"`
	Issued        string  `json:"issued"`
	VolumeEntered int64   `json:"volumeEntered"`
	Range         string  `json:"range"`
}

var SpecialRegions = []struct {
	name     string
	stations []int64
}{
	{
		// 10000002
		name:     "jita",
		stations: []int64{60003466, 60003760, 60003757, 60000361, 60000451, 60004423, 60002959, 60003460, 60003055, 60003469, 60000364, 60002953, 60000463, 60003463},
	}, {
		// 10000043
		name:     "amarr",
		stations: []int64{60008950, 60002569, 60008494},
	}, {
		// 10000032
		name:     "dodixie",
		stations: []int64{60011866, 60001867},
	}, {
		// 10000042
		name:     "hek",
		stations: []int64{60005236, 60004516, 60015140, 60005686, 60011287, 60005236},
	},
}

type PriceFetcher struct {
	db      evepraisal.PriceDB
	client  *pester.Client
	baseURL string

	stop chan bool
	wg   *sync.WaitGroup
}

func NewPriceFetcher(priceDB evepraisal.PriceDB, baseURL string, client *pester.Client) (*PriceFetcher, error) {

	p := &PriceFetcher{
		db:      priceDB,
		client:  client,
		baseURL: baseURL,

		stop: make(chan bool),
		wg:   &sync.WaitGroup{},
	}

	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		for {
			start := time.Now()
			p.runOnce()
			select {
			case <-time.After((5 * time.Minute) - time.Since(start)):
			case <-p.stop:
				return
			}
		}
	}()

	return p, nil
}

func (p *PriceFetcher) Close() error {
	close(p.stop)
	p.wg.Wait()
	return nil
}

func regionNames() []string {
	regions := make([]string, len(SpecialRegions)+1)
	regions[0] = "universe"
	for i, region := range SpecialRegions {
		regions[i+1] = region.name
	}
	return regions
}

func (p *PriceFetcher) runOnce() {
	log.Println("Fetch market data")
	priceMap, err := p.FetchOrderData(p.client, p.baseURL, []int{10000002, 10000042, 10000027, 10000032, 10000043})
	if err != nil {
		log.Println("ERROR: fetching market data: ", err)
		return
	}

	pricesFromCCP, err := p.FetchPriceData(p.client, p.baseURL)
	if err != nil {
		log.Println("ERROR: fetching CCP price data: ", err)
		return
	}

	for _, regionName := range regionNames() {
		for typeID, prices := range pricesFromCCP {
			p, ok := priceMap[regionName][typeID]
			if !ok || p.All.Volume < 50 {
				priceMap[regionName][typeID] = prices
			}
		}
	}

	for market, pmap := range priceMap {
		// this takes awhile, so let's check to see if we should stop between markets
		select {
		case <-p.stop:
			return
		default:
		}

		for itemName, price := range pmap {
			err = p.db.UpdatePrice(market, itemName, price)
			if err != nil {
				log.Printf("Error when updating price: %s", err)
			}
		}
	}
	log.Println("Done fetching market data")
}

func (p *PriceFetcher) freshPriceMap() map[string]map[int64]evepraisal.Prices {
	priceMap := make(map[string]map[int64]evepraisal.Prices)
	for _, region := range SpecialRegions {
		priceMap[region.name] = make(map[int64]evepraisal.Prices)
	}
	priceMap["universe"] = make(map[int64]evepraisal.Prices)
	return priceMap
}

func (p *PriceFetcher) FetchPriceData(client *pester.Client, baseURL string) (map[int64]evepraisal.Prices, error) {
	start := time.Now()
	url := fmt.Sprintf("%s/markets/prices/?datasource=tranquility", baseURL)
	esiPrices := make([]struct {
		TypeID        int64   `json:"type_id"`
		AveragePrice  float64 `json:"average_price"`
		AdjustedPrice float64 `json:"adjusted_price"`
	}, 0)
	err := fetchURL(client, url, &esiPrices)
	if err != nil {
		return nil, err
	}

	allPrices := make(map[int64]evepraisal.Prices, len(esiPrices))
	for _, p := range esiPrices {
		stats := evepraisal.PriceStats{
			Average:    p.AveragePrice,
			Max:        p.AdjustedPrice,
			Median:     p.AdjustedPrice,
			Min:        p.AdjustedPrice,
			Percentile: p.AdjustedPrice,
		}
		allPrices[p.TypeID] = evepraisal.Prices{
			All:      stats,
			Buy:      stats,
			Sell:     stats,
			Updated:  start,
			Strategy: "ccp",
		}
	}
	return allPrices, nil
}

func (p *PriceFetcher) FetchOrderData(client *pester.Client, baseURL string, regionIDs []int) (map[string]map[int64]evepraisal.Prices, error) {
	allOrdersByType := make(map[int64][]MarketOrder)
	finished := make(chan bool, 1)
	workerStop := make(chan bool, 1)
	errChannel := make(chan error, 1)
	fetchStart := time.Now()

	l := &sync.Mutex{}
	requestAndProcess := func(url string) (bool, error) {
		var orders []MarketOrder
		err := fetchURL(client, url, &orders)
		if err != nil {
			return false, err
		}

		l.Lock()
		for _, order := range orders {
			allOrdersByType[order.Type] = append(allOrdersByType[order.Type], order)
		}
		l.Unlock()
		if len(orders) == 0 {
			return false, nil
		}
		return true, nil
	}

	wg := &sync.WaitGroup{}
	for _, regionID := range regionIDs {
		wg.Add(1)
		go func(regionID int) {
			defer wg.Done()
			page := 1
			for {
				select {
				case <-workerStop:
					return
				default:
				}

				url := fmt.Sprintf("%s/markets/%d/orders/?datasource=tranquility&order_type=all&page=%d", baseURL, regionID, page)
				hasMore, err := requestAndProcess(url)
				if err != nil {
					errChannel <- fmt.Errorf("Failed to fetch market orders: %s", err)
					return
				}

				if !hasMore {
					break
				}
				page++
			}
		}(regionID)
	}

	go func() {
		wg.Wait()
		close(finished)
	}()

	select {
	case <-finished:
	case <-p.stop:
		close(workerStop)
		return nil, errors.New("Stopping during price fetch")
	case err := <-errChannel:
		if err != nil {
			close(workerStop)
			return nil, err
		}
	}

	log.Println("Performing aggregates on order data")
	// Calculate aggregates that we care about:
	newPriceMap := p.freshPriceMap()
	for k, orders := range allOrdersByType {
		for _, region := range SpecialRegions {
			filteredOrders := make([]MarketOrder, 0)
			ordercount := 0
			for _, order := range orders {
				matched := false
				for _, station := range region.stations {
					if station == order.StationID {
						matched = true
						ordercount++
						break
					}
				}
				if matched {
					filteredOrders = append(filteredOrders, order)
				}
			}
			agg := getPriceAggregatesForOrders(filteredOrders)
			agg.Updated = fetchStart
			agg.Strategy = "orders"
			newPriceMap[region.name][k] = agg
		}
		agg := getPriceAggregatesForOrders(orders)
		agg.Updated = fetchStart
		newPriceMap["universe"][k] = agg
	}

	log.Println("Finished performing aggregates on order data")

	return newPriceMap, nil
}
