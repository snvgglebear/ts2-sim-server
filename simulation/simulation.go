// Copyright (C) 2008-2018 by Nicolas Piganeau and the TS2 TEAM
// (See AUTHORS file)
//
// This program is free software; you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation; either version 2 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// along with this program; if not, write to the
// Free Software Foundation, Inc.,
// 59 Temple Place - Suite 330, Boston, MA  02111-1307, USA.

package simulation

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	log "gopkg.in/inconshreveable/log15.v2"
)

const timeStep = 500 * time.Millisecond

var logger log.Logger

// InitializeLogger creates the logger for the simulation module
func InitializeLogger(parentLogger log.Logger) {
	logger = parentLogger.New("module", "simulation")
}

// Simulation holds all the game logic.
type Simulation struct {
	SignalLib     SignalLibrary
	TrackItems    map[int]TrackItem
	Places        map[string]*Place
	Options       options
	Routes        map[int]*Route
	TrainTypes    map[string]*TrainType
	Services      map[string]*Service
	Trains        []*Train
	MessageLogger *MessageLogger
	EventChan     chan *Event

	clockTicker *time.Ticker
	stopChan    chan bool
}

// UnmarshalJSON for the Simulation type
func (sim *Simulation) UnmarshalJSON(data []byte) error {
	type auxItem map[string]json.RawMessage

	type auxSim struct {
		TrackItems    map[string]json.RawMessage
		Options       options
		SignalLib     SignalLibrary         `json:"signalLibrary"`
		Routes        map[string]*Route     `json:"routes"`
		TrainTypes    map[string]*TrainType `json:"trainTypes"`
		Services      map[string]*Service   `json:"services"`
		Trains        []*Train              `json:"trains"`
		MessageLogger *MessageLogger        `json:"messageLogger"`
	}

	var rawSim auxSim
	if err := json.Unmarshal(data, &rawSim); err != nil {
		return fmt.Errorf("unable to decode simulation JSON: %s", err)
	}
	sim.TrackItems = make(map[int]TrackItem)
	sim.Places = make(map[string]*Place)
	for tiId, tiString := range rawSim.TrackItems {
		var rawItem auxItem
		if err := json.Unmarshal(tiString, &rawItem); err != nil {
			return fmt.Errorf("unable to read TrackItem: %s. %s", tiString, err)
		}

		tiType := string(rawItem["__type__"])
		unmarshalItem := func(ti TrackItem) error {
			if err := json.Unmarshal(tiString, ti); err != nil {
				return fmt.Errorf("unable to decode %s: %s. %s", tiType, tiString, err)
			}
			tiId, errconv := strconv.Atoi(strings.Trim(tiId, `"`))
			if errconv != nil {
				return fmt.Errorf("unable to convert %s", errconv)
			}
			ti.setSimulation(sim)
			ti.setID(tiId)
			sim.TrackItems[tiId] = ti
			return nil
		}

		switch tiType {
		case `"LineItem"`:
			var ti LineItem
			unmarshalItem(&ti)
		case `"InvisibleLinkItem"`:
			var ti InvisibleLinkItem
			unmarshalItem(&ti)
		case `"EndItem"`:
			var ti EndItem
			unmarshalItem(&ti)
		case `"PlatformItem"`:
			var ti PlatformItem
			unmarshalItem(&ti)
		case `"TextItem"`:
			var ti TextItem
			unmarshalItem(&ti)
		case `"PointsItem"`:
			var ti PointsItem
			unmarshalItem(&ti)
		case `"SignalItem"`:
			var ti SignalItem
			unmarshalItem(&ti)
		case `"Place"`:
			var pl Place
			if err := json.Unmarshal(tiString, &pl); err != nil {
				return fmt.Errorf("unable to decode Place: %s. %s", tiString, err)
			}
			sim.Places[pl.PlaceCode] = &pl
		default:
			return fmt.Errorf("unknown TrackItem type: %s", rawItem["__type__"])
		}

	}
	if err := sim.checkTrackItemsLinks(); err != nil {
		return err
	}

	sim.Options = rawSim.Options
	sim.SignalLib = rawSim.SignalLib
	sim.Routes = make(map[int]*Route)
	for num, route := range rawSim.Routes {
		route.setSimulation(sim)
		route.initialize()
		routeNum, errRoute := strconv.Atoi(num)
		if errRoute != nil {
			return fmt.Errorf("routeNum : `%s` is invalid", num)
		}
		sim.Routes[routeNum] = route
	}
	sim.TrainTypes = rawSim.TrainTypes
	for _, tt := range sim.TrainTypes {
		tt.setSimulation(sim)
	}
	sim.Services = rawSim.Services
	for _, s := range sim.Services {
		s.setSimulation(sim)
	}
	sim.Trains = rawSim.Trains
	for _, t := range sim.Trains {
		t.setSimulation(sim)
	}
	sort.Slice(sim.Trains, func(i, j int) bool {
		switch {
		case len(sim.Trains[i].Service().Lines) == 0 && len(sim.Trains[j].Service().Lines) == 0:
			return sim.Trains[i].ServiceCode < sim.Trains[j].ServiceCode
		case len(sim.Trains[i].Service().Lines) == 0:
			return false
		case len(sim.Trains[j].Service().Lines) == 0:
			return true
		default:
			return sim.Trains[i].Service().Lines[0].ScheduledDepartureTime.Sub(
				sim.Trains[j].Service().Lines[0].ScheduledDepartureTime) < 0
		}
	})
	sim.MessageLogger = rawSim.MessageLogger
	sim.MessageLogger.setSimulation(sim)
	return nil
}

// Initialize initializes the simulation.
// This method must be called before Start.
func (sim *Simulation) Initialize() error {
	sim.MessageLogger.addMessage("Simulation initializing", softwareMsg)
	sim.EventChan = make(chan *Event)
	sim.stopChan = make(chan bool)
	return nil
}

// Start runs the main loop of the simulation by making the clock tick and process each object.
func (sim *Simulation) Start() {
	if sim.stopChan == nil || sim.EventChan == nil {
		panic("You must call Initialize before starting the simulation")
	}
	sim.clockTicker = time.NewTicker(timeStep)
	go sim.run()
	logger.Info("Simulation started")
}

// run enters the main loop of the simulation
func (sim *Simulation) run() {
	for {
		select {
		case <-sim.stopChan:
			sim.clockTicker.Stop()
			logger.Info("Simulation paused")
			return
		case <-sim.clockTicker.C:
			sim.increaseTime(timeStep)
			sim.sendEvent(&Event{ClockEvent, sim.Options.CurrentTime})
		}
	}
}

// Pause holds the simulation by stopping the clock ticker. Call Start again to restart the simulation.
func (sim *Simulation) Pause() {
	sim.stopChan <- true
}

// sendEvent sends the given event on the event channel to notify clients.
// Sending is done asynchronously so as not to block.
func (sim *Simulation) sendEvent(evt *Event) {
	go func() { sim.EventChan <- evt }()
}

// increaseTime adds the step to the simulation time.
func (sim *Simulation) increaseTime(step time.Duration) {
	sim.Options.CurrentTime.Lock()
	defer sim.Options.CurrentTime.Unlock()
	sim.Options.CurrentTime = sim.Options.CurrentTime.Add(step)
}

// checks that all TrackItems are linked together.
// Returns the first error met.
func (sim *Simulation) checkTrackItemsLinks() error {
	for _, ti := range sim.TrackItems {
		switch ti.Type() {
		case place, platformItem, textItem:
			continue
		case pointsItem:
			pi := ti.(*PointsItem)
			if pi.ReverseItem() == nil {
				return ItemNotLinkedAtError{item: ti, pt: pi.Reverse()}
			}
			fallthrough
		case lineItem, invisibleLinkItem, signalItem:
			if ti.NextItem() == nil {
				return ItemNotLinkedAtError{item: ti, pt: ti.End()}
			}
			fallthrough
		case endItem:
			if ti.PreviousItem() == nil {
				return ItemNotLinkedAtError{item: ti, pt: ti.Origin()}
			}
		}
	}
	return nil
}
