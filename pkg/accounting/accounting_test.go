// Copyright 2020 The Swarm Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package accounting_test

import (
	"context"
	"errors"
	"math/big"
	"sync"
	"testing"
	"time"

	"github.com/ethersphere/bee/pkg/accounting"
	"github.com/ethersphere/bee/pkg/log"
	"github.com/ethersphere/bee/pkg/p2p"
	p2pmock "github.com/ethersphere/bee/pkg/p2p/mock"
	"github.com/ethersphere/bee/pkg/statestore/mock"

	"github.com/ethersphere/bee/pkg/swarm"
)

const (
	testPrice       = uint64(10)
	testRefreshRate = int64(1000)
)

var (
	testPaymentTolerance = int64(10)
	testPaymentEarly     = int64(10)
	testPaymentThreshold = big.NewInt(10000)
)

type paymentCall struct {
	peer   swarm.Address
	amount *big.Int
}

// booking represents an accounting action and the expected result afterwards
type booking struct {
	peer              swarm.Address
	price             int64 // Credit if <0, Debit otherwise
	expectedBalance   int64
	originatedBalance int64
	originatedCredit  bool
	notifyPaymentSent bool
	overpay           uint64
}

func TestMutex(t *testing.T) {
	t.Run("locked mutex can not be locked again", func(t *testing.T) {
		m := accounting.NewMutex()
		m.Lock()

		var (
			c  chan struct{}
			wg sync.WaitGroup
		)

		wg.Add(1)
		go func() {
			wg.Done()
			m.Lock()
			c <- struct{}{}
		}()

		wg.Wait()

		select {
		case <-c:
			t.Error("not expected to acquire the lock")
		case <-time.After(time.Millisecond):
		}
	})

	t.Run("can lock after release", func(t *testing.T) {
		m := accounting.NewMutex()
		m.Lock()

		ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
		defer cancel()

		m.Unlock()
		if err := m.TryLock(ctx); err != nil {
			t.Errorf("expected no error, got %v", err)
		}
	})

	t.Run("locked mutex takes context into account", func(t *testing.T) {
		m := accounting.NewMutex()
		m.Lock()

		ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
		defer cancel()
		if err := m.TryLock(ctx); !errors.Is(err, accounting.ErrFailToLock) {
			t.Errorf("expected %v, got %v", accounting.ErrFailToLock, err)
		}
	})
}

// TestAccountingAddBalance does several accounting actions and verifies the balance after each steep
func TestAccountingAddBalance(t *testing.T) {
	logger := log.Noop

	store := mock.NewStateStore()
	defer store.Close()

	acc, err := accounting.NewAccounting(testPaymentThreshold, testPaymentTolerance, testPaymentEarly, logger, store, nil, big.NewInt(testRefreshRate), p2pmock.New())
	if err != nil {
		t.Fatal(err)
	}

	peer1Addr, err := swarm.ParseHexAddress("00112233")
	if err != nil {
		t.Fatal(err)
	}

	peer2Addr, err := swarm.ParseHexAddress("00112244")
	if err != nil {
		t.Fatal(err)
	}

	acc.Connect(peer1Addr)
	acc.Connect(peer2Addr)

	bookings := []booking{
		{peer: peer1Addr, price: 100, expectedBalance: 100},
		{peer: peer2Addr, price: 200, expectedBalance: 200},
		{peer: peer1Addr, price: 300, expectedBalance: 400},
		{peer: peer1Addr, price: -100, expectedBalance: 300},
		{peer: peer2Addr, price: -1000, expectedBalance: -800},
	}

	for i, booking := range bookings {
		if booking.price < 0 {
			creditAction, err := acc.PrepareCredit(context.Background(), booking.peer, uint64(-booking.price), true)
			if err != nil {
				t.Fatal(err)
			}
			err = creditAction.Apply()
			if err != nil {
				t.Fatal(err)
			}
			creditAction.Cleanup()
		} else {
			debitAction, err := acc.PrepareDebit(context.Background(), booking.peer, uint64(booking.price))
			if err != nil {
				t.Fatal(err)
			}
			err = debitAction.Apply()
			if err != nil {
				t.Fatal(err)
			}
			debitAction.Cleanup()
		}

		balance, err := acc.Balance(booking.peer)
		if err != nil {
			t.Fatal(err)
		}

		if balance.Int64() != booking.expectedBalance {
			t.Fatalf("balance for peer %v not as expected after booking %d. got %d, wanted %d", booking.peer.String(), i, balance, booking.expectedBalance)
		}
	}
}

// TestAccountingAddBalance does several accounting actions and verifies the balance after each steep
func TestAccountingAddOriginatedBalance(t *testing.T) {
	logger := log.Noop

	store := mock.NewStateStore()
	defer store.Close()

	acc, err := accounting.NewAccounting(testPaymentThreshold, testPaymentTolerance, testPaymentEarly, logger, store, nil, big.NewInt(testRefreshRate), p2pmock.New())
	if err != nil {
		t.Fatal(err)
	}

	f := func(ctx context.Context, peer swarm.Address, amount *big.Int, shadowBalance *big.Int) (*big.Int, int64, error) {
		return big.NewInt(0), 0, nil
	}

	acc.SetRefreshFunc(f)

	peer1Addr, err := swarm.ParseHexAddress("00112233")
	if err != nil {
		t.Fatal(err)
	}

	acc.Connect(peer1Addr)

	bookings := []booking{
		// originated credit
		{peer: peer1Addr, price: -2000, expectedBalance: -2000, originatedBalance: -2000, originatedCredit: true},
		// forwarder credit
		{peer: peer1Addr, price: -2000, expectedBalance: -4000, originatedBalance: -2000, originatedCredit: false},
		// inconsequential debit not moving balance closer to 0 than originbalance is to 0
		{peer: peer1Addr, price: 1000, expectedBalance: -3000, originatedBalance: -2000},
		// consequential debit moving balance closer to 0 than originbalance, therefore also moving originated balance along
		{peer: peer1Addr, price: 2000, expectedBalance: -1000, originatedBalance: -1000},
		// forwarder credit happening to increase debt
		{peer: peer1Addr, price: -7000, expectedBalance: -8000, originatedBalance: -1000, originatedCredit: false},
		// expect notifypaymentsent triggered by reserve that moves originated balance into positive domain because of earlier debit triggering overpay
		{peer: peer1Addr, price: -1000, expectedBalance: 1000, originatedBalance: 1000, overpay: 9000, notifyPaymentSent: true},
		// inconsequential debit because originated balance is in the positive domain
		{peer: peer1Addr, price: 1000, expectedBalance: 2000, originatedBalance: 1000},
		// originated credit moving the originated balance back into the negative domain, should be limited to the expectedbalance
		{peer: peer1Addr, price: -3000, expectedBalance: -1000, originatedBalance: -1000, originatedCredit: true},
	}

	paychan := make(chan struct{})

	for i, booking := range bookings {

		pay := func(ctx context.Context, peer swarm.Address, amount *big.Int) {
			if booking.overpay != 0 {
				debitAction, err := acc.PrepareDebit(context.Background(), peer, booking.overpay)
				if err != nil {
					t.Fatal(err)
				}
				_ = debitAction.Apply()
			}

			acc.NotifyPaymentSent(peer, amount, nil)
			paychan <- struct{}{}
		}

		acc.SetPayFunc(pay)

		if booking.price < 0 {
			creditAction, err := acc.PrepareCredit(context.Background(), booking.peer, uint64(-booking.price), booking.originatedCredit)
			if err != nil {
				t.Fatal(err)
			}

			if booking.notifyPaymentSent {
				select {
				case <-paychan:
				case <-time.After(1 * time.Second):
					t.Fatal("expected payment sent")
				}
			}

			err = creditAction.Apply()
			if err != nil {
				t.Fatal(err)
			}
			creditAction.Cleanup()
		} else {
			debitAction, err := acc.PrepareDebit(context.Background(), booking.peer, uint64(booking.price))
			if err != nil {
				t.Fatal(err)
			}
			err = debitAction.Apply()
			if err != nil {
				t.Fatal(err)
			}
			debitAction.Cleanup()
		}

		balance, err := acc.Balance(booking.peer)
		if err != nil {
			t.Fatal(err)
		}

		if balance.Int64() != booking.expectedBalance {
			t.Fatalf("balance for peer %v not as expected after booking %d. got %d, wanted %d", booking.peer.String(), i, balance, booking.expectedBalance)
		}

		originatedBalance, err := acc.OriginatedBalance(booking.peer)
		if err != nil {
			t.Fatal(err)
		}

		if originatedBalance.Int64() != booking.originatedBalance {
			t.Fatalf("originated balance for peer %v not as expected after booking %d. got %d, wanted %d", booking.peer.String(), i, originatedBalance, booking.originatedBalance)
		}
	}
}

// TestAccountingAdd_persistentBalances tests that balances are actually persisted
// It creates an accounting instance, does some accounting
// Then it creates a new accounting instance with the same store and verifies the balances
func TestAccountingAdd_persistentBalances(t *testing.T) {
	logger := log.Noop

	store := mock.NewStateStore()
	defer store.Close()

	acc, err := accounting.NewAccounting(testPaymentThreshold, testPaymentTolerance, testPaymentEarly, logger, store, nil, big.NewInt(testRefreshRate), p2pmock.New())
	if err != nil {
		t.Fatal(err)
	}

	peer1Addr, err := swarm.ParseHexAddress("00112233")
	if err != nil {
		t.Fatal(err)
	}

	peer2Addr, err := swarm.ParseHexAddress("00112244")
	if err != nil {
		t.Fatal(err)
	}

	acc.Connect(peer1Addr)
	acc.Connect(peer2Addr)

	peer1DebitAmount := testPrice
	debitAction, err := acc.PrepareDebit(context.Background(), peer1Addr, peer1DebitAmount)
	if err != nil {
		t.Fatal(err)
	}
	err = debitAction.Apply()
	if err != nil {
		t.Fatal(err)
	}
	debitAction.Cleanup()

	peer2CreditAmount := 2 * testPrice
	creditAction, err := acc.PrepareCredit(context.Background(), peer2Addr, peer2CreditAmount, true)
	if err != nil {
		t.Fatal(err)
	}
	if err = creditAction.Apply(); err != nil {
		t.Fatal(err)
	}
	creditAction.Cleanup()

	acc, err = accounting.NewAccounting(testPaymentThreshold, testPaymentTolerance, testPaymentEarly, logger, store, nil, big.NewInt(testRefreshRate), p2pmock.New())
	if err != nil {
		t.Fatal(err)
	}

	peer1Balance, err := acc.Balance(peer1Addr)
	if err != nil {
		t.Fatal(err)
	}

	if peer1Balance.Uint64() != peer1DebitAmount {
		t.Fatalf("peer1Balance not loaded correctly. got %d, wanted %d", peer1Balance, peer1DebitAmount)
	}

	peer2Balance, err := acc.Balance(peer2Addr)
	if err != nil {
		t.Fatal(err)
	}

	if peer2Balance.Int64() != -int64(peer2CreditAmount) {
		t.Fatalf("peer2Balance not loaded correctly. got %d, wanted %d", peer2Balance, -int64(peer2CreditAmount))
	}
}

// TestAccountingReserve tests that reserve returns an error if the payment threshold would be exceeded
func TestAccountingReserve(t *testing.T) {
	logger := log.Noop

	store := mock.NewStateStore()
	defer store.Close()

	acc, err := accounting.NewAccounting(testPaymentThreshold, testPaymentTolerance, testPaymentEarly, logger, store, nil, big.NewInt(testRefreshRate), p2pmock.New())
	if err != nil {
		t.Fatal(err)
	}

	peer1Addr, err := swarm.ParseHexAddress("00112233")
	if err != nil {
		t.Fatal(err)
	}

	acc.Connect(peer1Addr)

	_, err = acc.PrepareCredit(context.Background(), peer1Addr, testPaymentThreshold.Uint64()+1, true)
	if err == nil {
		t.Fatal("expected error from reserve")
	}

	if !errors.Is(err, accounting.ErrOverdraft) {
		t.Fatalf("expected overdraft error from reserve, got %v", err)
	}
}

// TestAccountingDisconnect tests that exceeding the disconnect threshold with Debit returns a p2p.DisconnectError
func TestAccountingDisconnect(t *testing.T) {
	logger := log.Noop

	store := mock.NewStateStore()
	defer store.Close()

	acc, err := accounting.NewAccounting(testPaymentThreshold, testPaymentTolerance, testPaymentEarly, logger, store, nil, big.NewInt(testRefreshRate), p2pmock.New())
	if err != nil {
		t.Fatal(err)
	}

	peer1Addr, err := swarm.ParseHexAddress("00112233")
	if err != nil {
		t.Fatal(err)
	}

	acc.Connect(peer1Addr)

	// put the peer 1 unit away from disconnect
	debitAction, err := acc.PrepareDebit(context.Background(), peer1Addr, (testPaymentThreshold.Uint64()*(100+uint64(testPaymentTolerance))/100)-1)
	if err != nil {
		t.Fatal(err)
	}
	err = debitAction.Apply()
	if err != nil {
		t.Fatal("expected no error while still within tolerance")
	}
	debitAction.Cleanup()

	// put the peer over thee threshold
	debitAction, err = acc.PrepareDebit(context.Background(), peer1Addr, 1)
	if err != nil {
		t.Fatal(err)
	}
	err = debitAction.Apply()
	if err == nil {
		t.Fatal("expected Add to return error")
	}
	debitAction.Cleanup()

	var e *p2p.BlockPeerError
	if !errors.As(err, &e) {
		t.Fatalf("expected BlockPeerError, got %v", err)
	}
}

// TestAccountingCallSettlement tests that settlement is called correctly if the payment threshold is hit
func TestAccountingCallSettlement(t *testing.T) {
	logger := log.Noop

	store := mock.NewStateStore()
	defer store.Close()

	acc, err := accounting.NewAccounting(testPaymentThreshold, testPaymentTolerance, testPaymentEarly, logger, store, nil, big.NewInt(testRefreshRate), p2pmock.New())
	if err != nil {
		t.Fatal(err)
	}

	refreshchan := make(chan paymentCall, 1)

	f := func(ctx context.Context, peer swarm.Address, amount *big.Int, shadowBalance *big.Int) (*big.Int, int64, error) {
		refreshchan <- paymentCall{peer: peer, amount: amount}
		return amount, 0, nil
	}

	pay := func(ctx context.Context, peer swarm.Address, amount *big.Int) {
	}

	acc.SetRefreshFunc(f)
	acc.SetPayFunc(pay)

	peer1Addr, err := swarm.ParseHexAddress("00112233")
	if err != nil {
		t.Fatal(err)
	}

	acc.Connect(peer1Addr)

	requestPrice := testPaymentThreshold.Uint64() - 1000

	creditAction, err := acc.PrepareCredit(context.Background(), peer1Addr, requestPrice, true)
	if err != nil {
		t.Fatal(err)
	}

	// Credit until payment treshold
	err = creditAction.Apply()
	if err != nil {
		t.Fatal(err)
	}

	creditAction.Cleanup()

	// try another request
	creditAction, err = acc.PrepareCredit(context.Background(), peer1Addr, 1, true)
	if err != nil {
		t.Fatal(err)
	}

	select {
	case call := <-refreshchan:
		if call.amount.Cmp(big.NewInt(int64(requestPrice))) != 0 {
			t.Fatalf("paid wrong amount. got %d wanted %d", call.amount, requestPrice)
		}
		if !call.peer.Equal(peer1Addr) {
			t.Fatalf("wrong peer address got %v wanted %v", call.peer, peer1Addr)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("timeout waiting for payment")
	}

	if acc.IsPaymentOngoing(peer1Addr) {
		t.Fatal("triggered monetary settlement")
	}

	creditAction.Cleanup()

	balance, err := acc.Balance(peer1Addr)
	if err != nil {
		t.Fatal(err)
	}
	if balance.Int64() != 0 {
		t.Fatalf("expected balance to be reset. got %d", balance)
	}

	// Assume 100 is reserved by some other request
	creditActionLong, err := acc.PrepareCredit(context.Background(), peer1Addr, 100, true)
	if err != nil {
		t.Fatal(err)
	}

	// Credit until the expected debt exceeds payment threshold
	expectedAmount := testPaymentThreshold.Uint64() - 101
	creditAction, err = acc.PrepareCredit(context.Background(), peer1Addr, expectedAmount, true)
	if err != nil {
		t.Fatal(err)
	}

	err = creditAction.Apply()
	if err != nil {
		t.Fatal(err)
	}

	creditAction.Cleanup()

	// try another request to trigger settlement
	creditAction, err = acc.PrepareCredit(context.Background(), peer1Addr, 1, true)
	if err != nil {
		t.Fatal(err)
	}

	creditAction.Cleanup()

	select {
	case call := <-refreshchan:
		if call.amount.Cmp(big.NewInt(int64(expectedAmount))) != 0 {
			t.Fatalf("paid wrong amount. got %d wanted %d", call.amount, expectedAmount)
		}
		if !call.peer.Equal(peer1Addr) {
			t.Fatalf("wrong peer address got %v wanted %v", call.peer, peer1Addr)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("timeout waiting for payment")
	}

	if acc.IsPaymentOngoing(peer1Addr) {
		t.Fatal("triggered monetary settlement")
	}
	creditActionLong.Cleanup()
}

func TestAccountingCallSettlementMonetary(t *testing.T) {
	logger := log.Noop

	store := mock.NewStateStore()
	defer store.Close()

	acc, err := accounting.NewAccounting(testPaymentThreshold, testPaymentTolerance, testPaymentEarly, logger, store, nil, big.NewInt(testRefreshRate), p2pmock.New())
	if err != nil {
		t.Fatal(err)
	}

	refreshchan := make(chan paymentCall, 1)
	paychan := make(chan paymentCall, 1)

	notTimeSettledAmount := big.NewInt(testRefreshRate * 2)

	acc.SetRefreshFunc(func(ctx context.Context, peer swarm.Address, amount *big.Int, shadowBalance *big.Int) (*big.Int, int64, error) {
		refreshchan <- paymentCall{peer: peer, amount: amount}
		return new(big.Int).Sub(amount, notTimeSettledAmount), 0, nil
	})

	acc.SetPayFunc(func(ctx context.Context, peer swarm.Address, amount *big.Int) {
		paychan <- paymentCall{peer: peer, amount: amount}
	})

	peer1Addr, err := swarm.ParseHexAddress("00112233")
	if err != nil {
		t.Fatal(err)
	}

	acc.Connect(peer1Addr)

	requestPrice := testPaymentThreshold.Uint64() - 1000

	creditAction, err := acc.PrepareCredit(context.Background(), peer1Addr, requestPrice, true)
	if err != nil {
		t.Fatal(err)
	}

	// Credit until payment treshold
	err = creditAction.Apply()
	if err != nil {
		t.Fatal(err)
	}

	creditAction.Cleanup()

	// try another request
	creditAction, err = acc.PrepareCredit(context.Background(), peer1Addr, 1, true)
	if err != nil {
		t.Fatal(err)
	}

	select {
	case call := <-refreshchan:
		if call.amount.Cmp(big.NewInt(int64(requestPrice))) != 0 {
			t.Fatalf("paid wrong amount. got %d wanted %d", call.amount, requestPrice)
		}
		if !call.peer.Equal(peer1Addr) {
			t.Fatalf("wrong peer address got %v wanted %v", call.peer, peer1Addr)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("timeout waiting for payment")
	}

	select {
	case call := <-paychan:
		if call.amount.Cmp(notTimeSettledAmount) != 0 {
			t.Fatalf("paid wrong amount. got %d wanted %d", call.amount, notTimeSettledAmount)
		}
		if !call.peer.Equal(peer1Addr) {
			t.Fatalf("wrong peer address got %v wanted %v", call.peer, peer1Addr)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("timeout waiting for payment")
	}

	creditAction.Cleanup()

	balance, err := acc.Balance(peer1Addr)
	if err != nil {
		t.Fatal(err)
	}
	if balance.Cmp(new(big.Int).Neg(notTimeSettledAmount)) != 0 {
		t.Fatalf("expected balance to be adjusted. got %d", balance)
	}

	acc.SetRefreshFunc(func(ctx context.Context, peer swarm.Address, amount *big.Int, shadowBalance *big.Int) (*big.Int, int64, error) {
		refreshchan <- paymentCall{peer: peer, amount: amount}
		return big.NewInt(0), 0, nil
	})

	// Credit until the expected debt exceeds payment threshold
	expectedAmount := testPaymentThreshold.Uint64()

	_, err = acc.PrepareCredit(context.Background(), peer1Addr, expectedAmount, true)
	if !errors.Is(err, accounting.ErrOverdraft) {
		t.Fatalf("expected overdraft, got %v", err)
	}

	select {
	case call := <-refreshchan:
		if call.amount.Cmp(notTimeSettledAmount) != 0 {
			t.Fatalf("paid wrong amount. got %d wanted %d", call.amount, notTimeSettledAmount)
		}
		if !call.peer.Equal(peer1Addr) {
			t.Fatalf("wrong peer address got %v wanted %v", call.peer, peer1Addr)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("timeout waiting for refreshment")
	}

	select {
	case <-paychan:
		t.Fatal("pay called twice")
	case <-time.After(1 * time.Second):
	}
}

func TestAccountingCallSettlementTooSoon(t *testing.T) {
	logger := log.Noop

	store := mock.NewStateStore()
	defer store.Close()

	acc, err := accounting.NewAccounting(testPaymentThreshold, testPaymentTolerance, testPaymentEarly, logger, store, nil, big.NewInt(testRefreshRate), p2pmock.New())
	if err != nil {
		t.Fatal(err)
	}

	refreshchan := make(chan paymentCall, 1)
	paychan := make(chan paymentCall, 1)

	ts := int64(1000)

	acc.SetRefreshFunc(func(ctx context.Context, peer swarm.Address, amount *big.Int, shadowBalance *big.Int) (*big.Int, int64, error) {
		refreshchan <- paymentCall{peer: peer, amount: amount}
		return amount, ts, nil
	})

	acc.SetPayFunc(func(ctx context.Context, peer swarm.Address, amount *big.Int) {
		paychan <- paymentCall{peer: peer, amount: amount}
	})

	peer1Addr, err := swarm.ParseHexAddress("00112233")
	if err != nil {
		t.Fatal(err)
	}

	acc.Connect(peer1Addr)

	requestPrice := testPaymentThreshold.Uint64() - 1000

	creditAction, err := acc.PrepareCredit(context.Background(), peer1Addr, requestPrice, true)
	if err != nil {
		t.Fatal(err)
	}

	// Credit until payment treshold
	err = creditAction.Apply()
	if err != nil {
		t.Fatal(err)
	}

	creditAction.Cleanup()

	// try another request
	creditAction, err = acc.PrepareCredit(context.Background(), peer1Addr, 1, true)
	if err != nil {
		t.Fatal(err)
	}

	select {
	case call := <-refreshchan:
		if call.amount.Cmp(big.NewInt(int64(requestPrice))) != 0 {
			t.Fatalf("paid wrong amount. got %d wanted %d", call.amount, requestPrice)
		}
		if !call.peer.Equal(peer1Addr) {
			t.Fatalf("wrong peer address got %v wanted %v", call.peer, peer1Addr)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("timeout waiting for payment")
	}

	creditAction.Cleanup()

	balance, err := acc.Balance(peer1Addr)
	if err != nil {
		t.Fatal(err)
	}
	if balance.Cmp(big.NewInt(0)) != 0 {
		t.Fatalf("expected balance to be adjusted. got %d", balance)
	}

	acc.SetTime(ts)

	creditAction, err = acc.PrepareCredit(context.Background(), peer1Addr, requestPrice, true)
	if err != nil {
		t.Fatal(err)
	}

	// Credit until payment treshold
	err = creditAction.Apply()
	if err != nil {
		t.Fatal(err)
	}

	creditAction.Cleanup()

	// try another request
	creditAction, err = acc.PrepareCredit(context.Background(), peer1Addr, 1, true)
	if err != nil {
		t.Fatal(err)
	}

	select {
	case <-refreshchan:
		t.Fatal("sent refreshment")
	default:
	}

	select {
	case call := <-paychan:
		if call.amount.Cmp(big.NewInt(int64(requestPrice))) != 0 {
			t.Fatalf("paid wrong amount. got %d wanted %d", call.amount, requestPrice)
		}
		if !call.peer.Equal(peer1Addr) {
			t.Fatalf("wrong peer address got %v wanted %v", call.peer, peer1Addr)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("payment not sent")
	}

	creditAction.Cleanup()

	acc.NotifyPaymentSent(peer1Addr, big.NewInt(int64(requestPrice)), errors.New("error"))
	acc.SetTime(ts + 1)

	// try another request
	_, err = acc.PrepareCredit(context.Background(), peer1Addr, 1, true)
	if err != nil {
		t.Fatal(err)
	}

	select {
	case call := <-refreshchan:
		if call.amount.Cmp(big.NewInt(int64(requestPrice))) != 0 {
			t.Fatalf("paid wrong amount. got %d wanted %d", call.amount, requestPrice)
		}
		if !call.peer.Equal(peer1Addr) {
			t.Fatalf("wrong peer address got %v wanted %v", call.peer, peer1Addr)
		}
	default:
		t.Fatal("no refreshment")
	}
}

// TestAccountingCallSettlementEarly tests that settlement is called correctly if the payment threshold minus early payment is hit
func TestAccountingCallSettlementEarly(t *testing.T) {
	logger := log.Noop

	store := mock.NewStateStore()
	defer store.Close()

	debt := uint64(500)
	earlyPayment := int64(10)

	acc, err := accounting.NewAccounting(testPaymentThreshold, testPaymentTolerance, earlyPayment, logger, store, nil, big.NewInt(testRefreshRate), p2pmock.New())
	if err != nil {
		t.Fatal(err)
	}

	refreshchan := make(chan paymentCall, 1)

	f := func(ctx context.Context, peer swarm.Address, amount *big.Int, shadowBalance *big.Int) (*big.Int, int64, error) {
		refreshchan <- paymentCall{peer: peer, amount: amount}
		return amount, 0, nil
	}

	acc.SetRefreshFunc(f)

	peer1Addr, err := swarm.ParseHexAddress("00112233")
	if err != nil {
		t.Fatal(err)
	}

	acc.Connect(peer1Addr)

	creditAction, err := acc.PrepareCredit(context.Background(), peer1Addr, debt, true)
	if err != nil {
		t.Fatal(err)
	}
	if err = creditAction.Apply(); err != nil {
		t.Fatal(err)
	}
	creditAction.Cleanup()

	payment := testPaymentThreshold.Uint64() * (100 - uint64(earlyPayment)) / 100
	creditAction, err = acc.PrepareCredit(context.Background(), peer1Addr, payment, true)
	if err != nil {
		t.Fatal(err)
	}

	creditAction.Cleanup()

	select {
	case call := <-refreshchan:
		if call.amount.Cmp(big.NewInt(int64(debt))) != 0 {
			t.Fatalf("paid wrong amount. got %d wanted %d", call.amount, debt)
		}
		if !call.peer.Equal(peer1Addr) {
			t.Fatalf("wrong peer address got %v wanted %v", call.peer, peer1Addr)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("timeout waiting for payment")
	}

	balance, err := acc.Balance(peer1Addr)
	if err != nil {
		t.Fatal(err)
	}
	if balance.Int64() != 0 {
		t.Fatalf("expected balance to be reset. got %d", balance)
	}
}

func TestAccountingSurplusBalance(t *testing.T) {
	logger := log.Noop

	store := mock.NewStateStore()
	defer store.Close()

	acc, err := accounting.NewAccounting(testPaymentThreshold, 0, 0, logger, store, nil, big.NewInt(testRefreshRate), p2pmock.New())
	if err != nil {
		t.Fatal(err)
	}
	peer1Addr, err := swarm.ParseHexAddress("00112233")
	if err != nil {
		t.Fatal(err)
	}
	acc.Connect(peer1Addr)

	// Try Debiting a large amount to peer so balance is large positive
	debitAction, err := acc.PrepareDebit(context.Background(), peer1Addr, testPaymentThreshold.Uint64()-1)
	if err != nil {
		t.Fatal(err)
	}
	err = debitAction.Apply()
	if err != nil {
		t.Fatal(err)
	}
	debitAction.Cleanup()
	// Notify of incoming payment from same peer, so balance goes to 0 with surplusbalance 2
	err = acc.NotifyPaymentReceived(peer1Addr, new(big.Int).Add(testPaymentThreshold, big.NewInt(1)))
	if err != nil {
		t.Fatal("Unexpected overflow from doable NotifyPayment")
	}
	//sanity check surplus balance
	val, err := acc.SurplusBalance(peer1Addr)
	if err != nil {
		t.Fatal("Error checking Surplusbalance")
	}
	if val.Int64() != 2 {
		t.Fatal("Not expected surplus balance")
	}
	//sanity check balance
	val, err = acc.Balance(peer1Addr)
	if err != nil {
		t.Fatal("Error checking Balance")
	}
	if val.Int64() != 0 {
		t.Fatal("Not expected balance")
	}
	// Notify of incoming payment from same peer, so balance goes to 0 with surplusbalance 10002 (testpaymentthreshold+2)
	err = acc.NotifyPaymentReceived(peer1Addr, testPaymentThreshold)
	if err != nil {
		t.Fatal("Unexpected error from NotifyPayment")
	}
	//sanity check surplus balance
	val, err = acc.SurplusBalance(peer1Addr)
	if err != nil {
		t.Fatal("Error checking Surplusbalance")
	}
	if val.Int64() != testPaymentThreshold.Int64()+2 {
		t.Fatal("Unexpected surplus balance")
	}
	//sanity check balance
	val, err = acc.Balance(peer1Addr)
	if err != nil {
		t.Fatal("Error checking Balance")
	}
	if val.Int64() != 0 {
		t.Fatal("Not expected balance, expected 0")
	}
	// Debit for same peer, so balance stays 0 with surplusbalance decreasing to 2
	debitAction, err = acc.PrepareDebit(context.Background(), peer1Addr, testPaymentThreshold.Uint64())
	if err != nil {
		t.Fatal(err)
	}
	err = debitAction.Apply()
	if err != nil {
		t.Fatal("Unexpected error from Credit")
	}
	debitAction.Cleanup()
	// sanity check surplus balance
	val, err = acc.SurplusBalance(peer1Addr)
	if err != nil {
		t.Fatal("Error checking Surplusbalance")
	}
	if val.Int64() != 2 {
		t.Fatal("Unexpected surplus balance")
	}
	//sanity check balance
	val, err = acc.Balance(peer1Addr)
	if err != nil {
		t.Fatal("Error checking Balance")
	}
	if val.Int64() != 0 {
		t.Fatal("Not expected balance, expected 0")
	}
	// Debit for same peer, so balance goes to 9998 (testpaymentthreshold - 2) with surplusbalance decreasing to 0
	debitAction, err = acc.PrepareDebit(context.Background(), peer1Addr, testPaymentThreshold.Uint64())
	if err != nil {
		t.Fatal(err)
	}
	err = debitAction.Apply()
	if err != nil {
		t.Fatal("Unexpected error from Debit")
	}
	debitAction.Cleanup()
	// sanity check surplus balance
	val, err = acc.SurplusBalance(peer1Addr)
	if err != nil {
		t.Fatal("Error checking Surplusbalance")
	}
	if val.Int64() != 0 {
		t.Fatal("Unexpected surplus balance")
	}
	//sanity check balance
	val, err = acc.Balance(peer1Addr)
	if err != nil {
		t.Fatal("Error checking Balance")
	}
	if val.Int64() != testPaymentThreshold.Int64()-2 {
		t.Fatal("Not expected balance, expected 0")
	}
}

// TestAccountingNotifyPayment tests that payments adjust the balance
func TestAccountingNotifyPaymentReceived(t *testing.T) {
	logger := log.Noop

	store := mock.NewStateStore()
	defer store.Close()

	acc, err := accounting.NewAccounting(testPaymentThreshold, testPaymentTolerance, testPaymentEarly, logger, store, nil, big.NewInt(testRefreshRate), p2pmock.New())
	if err != nil {
		t.Fatal(err)
	}

	peer1Addr, err := swarm.ParseHexAddress("00112233")
	if err != nil {
		t.Fatal(err)
	}

	acc.Connect(peer1Addr)

	debtAmount := uint64(100)
	debitAction, err := acc.PrepareDebit(context.Background(), peer1Addr, debtAmount)
	if err != nil {
		t.Fatal(err)
	}
	err = debitAction.Apply()
	if err != nil {
		t.Fatal(err)
	}
	debitAction.Cleanup()

	err = acc.NotifyPaymentReceived(peer1Addr, new(big.Int).SetUint64(debtAmount))
	if err != nil {
		t.Fatal(err)
	}

	debitAction, err = acc.PrepareDebit(context.Background(), peer1Addr, debtAmount)
	if err != nil {
		t.Fatal(err)
	}
	err = debitAction.Apply()
	if err != nil {
		t.Fatal(err)
	}
	debitAction.Cleanup()

	err = acc.NotifyPaymentReceived(peer1Addr, new(big.Int).SetUint64(debtAmount))
	if err != nil {
		t.Fatal(err)
	}
}

type pricingMock struct {
	called           bool
	peer             swarm.Address
	paymentThreshold *big.Int
}

func (p *pricingMock) AnnouncePaymentThreshold(ctx context.Context, peer swarm.Address, paymentThreshold *big.Int) error {
	p.called = true
	p.peer = peer
	p.paymentThreshold = paymentThreshold
	return nil
}

func TestAccountingConnected(t *testing.T) {
	logger := log.Noop

	store := mock.NewStateStore()
	defer store.Close()

	pricing := &pricingMock{}

	_, err := accounting.NewAccounting(testPaymentThreshold, testPaymentTolerance, testPaymentEarly, logger, store, pricing, big.NewInt(testRefreshRate), p2pmock.New())
	if err != nil {
		t.Fatal(err)
	}

	peer1Addr, err := swarm.ParseHexAddress("00112233")
	if err != nil {
		t.Fatal(err)
	}

	err = pricing.AnnouncePaymentThreshold(context.Background(), peer1Addr, testPaymentThreshold)
	if err != nil {
		t.Fatal(err)
	}

	if !pricing.called {
		t.Fatal("expected pricing to be called")
	}

	if !pricing.peer.Equal(peer1Addr) {
		t.Fatalf("paid to wrong peer. got %v wanted %v", pricing.peer, peer1Addr)
	}

	if pricing.paymentThreshold != testPaymentThreshold {
		t.Fatalf("paid wrong amount. got %d wanted %d", pricing.paymentThreshold, testPaymentThreshold)
	}
}

func TestAccountingNotifyPaymentThreshold(t *testing.T) {
	logger := log.Noop

	store := mock.NewStateStore()
	defer store.Close()

	pricing := &pricingMock{}

	acc, err := accounting.NewAccounting(testPaymentThreshold, testPaymentTolerance, 0, logger, store, pricing, big.NewInt(testRefreshRate), p2pmock.New())
	if err != nil {
		t.Fatal(err)
	}

	refreshchan := make(chan paymentCall, 1)

	f := func(ctx context.Context, peer swarm.Address, amount *big.Int, shadowBalance *big.Int) (*big.Int, int64, error) {
		refreshchan <- paymentCall{peer: peer, amount: amount}
		return amount, 0, nil
	}

	acc.SetRefreshFunc(f)

	peer1Addr, err := swarm.ParseHexAddress("00112233")
	if err != nil {
		t.Fatal(err)
	}

	acc.Connect(peer1Addr)

	debt := uint64(50)
	lowerThreshold := uint64(100)

	err = acc.NotifyPaymentThreshold(peer1Addr, new(big.Int).SetUint64(lowerThreshold))
	if err != nil {
		t.Fatal(err)
	}

	creditAction, err := acc.PrepareCredit(context.Background(), peer1Addr, debt, true)
	if err != nil {
		t.Fatal(err)
	}
	if err = creditAction.Apply(); err != nil {
		t.Fatal(err)
	}
	creditAction.Cleanup()

	_, err = acc.PrepareCredit(context.Background(), peer1Addr, lowerThreshold, true)
	if err != nil {
		t.Fatal(err)
	}

	select {
	case call := <-refreshchan:
		if call.amount.Cmp(big.NewInt(int64(debt))) != 0 {
			t.Fatalf("paid wrong amount. got %d wanted %d", call.amount, debt)
		}
		if !call.peer.Equal(peer1Addr) {
			t.Fatalf("wrong peer address got %v wanted %v", call.peer, peer1Addr)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("timeout waiting for payment")
	}
}

func TestAccountingPeerDebt(t *testing.T) {
	logger := log.Noop

	store := mock.NewStateStore()
	defer store.Close()

	pricing := &pricingMock{}

	acc, err := accounting.NewAccounting(testPaymentThreshold, testPaymentTolerance, 0, logger, store, pricing, big.NewInt(testRefreshRate), p2pmock.New())
	if err != nil {
		t.Fatal(err)
	}

	peer1Addr := swarm.MustParseHexAddress("00112233")
	acc.Connect(peer1Addr)

	debt := uint64(1000)
	debitAction, err := acc.PrepareDebit(context.Background(), peer1Addr, debt)
	if err != nil {
		t.Fatal(err)
	}
	err = debitAction.Apply()
	if err != nil {
		t.Fatal(err)
	}
	debitAction.Cleanup()
	actualDebt, err := acc.PeerDebt(peer1Addr)
	if err != nil {
		t.Fatal(err)
	}
	if actualDebt.Cmp(new(big.Int).SetUint64(debt)) != 0 {
		t.Fatalf("wrong actual debt. got %d wanted %d", actualDebt, debt)
	}

	peer2Addr := swarm.MustParseHexAddress("11112233")
	acc.Connect(peer2Addr)
	creditAction, err := acc.PrepareCredit(context.Background(), peer2Addr, 500, true)
	if err != nil {
		t.Fatal(err)
	}
	if err = creditAction.Apply(); err != nil {
		t.Fatal(err)
	}
	creditAction.Cleanup()
	actualDebt, err = acc.PeerDebt(peer2Addr)
	if err != nil {
		t.Fatal(err)
	}
	if actualDebt.Cmp(big.NewInt(0)) != 0 {
		t.Fatalf("wrong actual debt. got %d wanted 0", actualDebt)
	}

	peer3Addr := swarm.MustParseHexAddress("22112233")
	actualDebt, err = acc.PeerDebt(peer3Addr)
	if err != nil {
		t.Fatal(err)
	}
	if actualDebt.Cmp(big.NewInt(0)) != 0 {
		t.Fatalf("wrong actual debt. got %d wanted 0", actualDebt)
	}
}

func TestAccountingCallPaymentErrorRetries(t *testing.T) {
	logger := log.Noop

	store := mock.NewStateStore()
	defer store.Close()

	acc, err := accounting.NewAccounting(testPaymentThreshold, testPaymentTolerance, testPaymentEarly, logger, store, nil, big.NewInt(1), p2pmock.New())
	if err != nil {
		t.Fatal(err)
	}

	refreshchan := make(chan paymentCall, 1)
	paychan := make(chan paymentCall, 1)

	ts := int64(100)
	acc.SetTime(ts)

	acc.SetRefreshFunc(func(ctx context.Context, peer swarm.Address, amount *big.Int, shadowBalance *big.Int) (*big.Int, int64, error) {
		refreshchan <- paymentCall{peer: peer, amount: big.NewInt(1)}
		return big.NewInt(1), ts, nil
	})

	acc.SetPayFunc(func(ctx context.Context, peer swarm.Address, amount *big.Int) {
		paychan <- paymentCall{peer: peer, amount: amount}
	})

	peer1Addr, err := swarm.ParseHexAddress("00112233")
	if err != nil {
		t.Fatal(err)
	}
	acc.Connect(peer1Addr)

	requestPrice := testPaymentThreshold.Uint64() - 100

	// Credit until near payment threshold
	creditAction, err := acc.PrepareCredit(context.Background(), peer1Addr, requestPrice, true)
	if err != nil {
		t.Fatal(err)
	}
	if err = creditAction.Apply(); err != nil {
		t.Fatal(err)
	}
	creditAction.Cleanup()

	creditAction, err = acc.PrepareCredit(context.Background(), peer1Addr, 2, true)
	if err != nil {
		t.Fatal(err)
	}

	select {
	case <-refreshchan:
	case <-time.After(1 * time.Second):
		t.Fatalf("expected refreshment")
	}

	var sentAmount *big.Int
	select {
	case call := <-paychan:
		sentAmount = call.amount
	case <-time.After(1 * time.Second):
		t.Fatal("payment expected to be sent")

	}

	creditAction.Cleanup()

	acc.NotifyPaymentSent(peer1Addr, sentAmount, errors.New("error"))

	// try another n requests 1 per second
	for i := 0; i < 10; i++ {
		ts++
		acc.SetTime(ts)
		creditAction, err = acc.PrepareCredit(context.Background(), peer1Addr, 2, true)
		if err != nil {
			t.Fatal(err)
		}

		select {
		case <-refreshchan:
		case <-time.After(1 * time.Second):
			t.Fatal("expected refreshment")
		}

		if acc.IsPaymentOngoing(peer1Addr) {
			t.Fatal("unexpected ongoing payment")
		}

		creditAction.Cleanup()
	}

	ts++
	acc.SetTime(ts)

	// try another request
	creditAction, err = acc.PrepareCredit(context.Background(), peer1Addr, 1, true)
	if err != nil {
		t.Fatal(err)
	}

	select {
	case <-refreshchan:
	case <-time.After(1 * time.Second):
		t.Fatalf("expected refreshment")

	}

	select {
	case <-paychan:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("payment expected to be sent")
	}

	creditAction.Cleanup()
}

var errInvalidReason = errors.New("invalid blocklist reason")

func TestAccountingGhostOverdraft(t *testing.T) {
	logger := log.Noop

	store := mock.NewStateStore()
	defer store.Close()

	var blocklistTime int64

	paymentThresholdInRefreshmentSeconds := new(big.Int).Div(testPaymentThreshold, big.NewInt(testRefreshRate)).Uint64()

	f := func(s swarm.Address, t time.Duration, reason string) error {
		if reason != "ghost overdraw" {
			return errInvalidReason
		}
		blocklistTime = int64(t.Seconds())
		return nil
	}

	acc, err := accounting.NewAccounting(testPaymentThreshold, testPaymentTolerance, testPaymentEarly, logger, store, nil, big.NewInt(testRefreshRate), p2pmock.New(p2pmock.WithBlocklistFunc(f)))
	if err != nil {
		t.Fatal(err)
	}

	ts := int64(1000)
	acc.SetTime(ts)

	peer, err := swarm.ParseHexAddress("00112233")
	if err != nil {
		t.Fatal(err)
	}
	acc.Connect(peer)

	requestPrice := testPaymentThreshold.Uint64()

	debitActionNormal, err := acc.PrepareDebit(context.Background(), peer, requestPrice)
	if err != nil {
		t.Fatal(err)
	}
	err = debitActionNormal.Apply()
	if err != nil {
		t.Fatal(err)
	}
	debitActionNormal.Cleanup()

	// debit ghost balance
	debitActionGhost, err := acc.PrepareDebit(context.Background(), peer, requestPrice)
	if err != nil {
		t.Fatal(err)
	}
	debitActionGhost.Cleanup()

	// increase shadow reserve
	debitActionShadow, err := acc.PrepareDebit(context.Background(), peer, requestPrice)
	if err != nil {
		t.Fatal(err)
	}
	_ = debitActionShadow

	if blocklistTime != 0 {
		t.Fatal("unexpected blocklist")
	}

	// ghost overdraft triggering blocklist
	debitAction4, err := acc.PrepareDebit(context.Background(), peer, requestPrice)
	if err != nil {
		t.Fatal(err)
	}
	debitAction4.Cleanup()

	if blocklistTime != int64(5*paymentThresholdInRefreshmentSeconds) {
		t.Fatalf("unexpected blocklisting time, got %v expected %v", blocklistTime, 5*paymentThresholdInRefreshmentSeconds)
	}
}

func TestAccountingReconnectBeforeAllowed(t *testing.T) {
	logger := log.Noop

	store := mock.NewStateStore()
	defer store.Close()

	var blocklistTime int64

	paymentThresholdInRefreshmentSeconds := new(big.Int).Div(testPaymentThreshold, big.NewInt(testRefreshRate)).Uint64()

	f := func(s swarm.Address, t time.Duration, reason string) error {
		if reason != "disconnected" {
			return errInvalidReason
		}
		blocklistTime = int64(t.Seconds())
		return nil
	}

	acc, err := accounting.NewAccounting(testPaymentThreshold, testPaymentTolerance, testPaymentEarly, logger, store, nil, big.NewInt(testRefreshRate), p2pmock.New(p2pmock.WithBlocklistFunc(f)))
	if err != nil {
		t.Fatal(err)
	}

	ts := int64(1000)
	acc.SetTime(ts)

	peer, err := swarm.ParseHexAddress("00112233")
	if err != nil {
		t.Fatal(err)
	}
	acc.Connect(peer)

	requestPrice := testPaymentThreshold.Uint64()

	debitActionNormal, err := acc.PrepareDebit(context.Background(), peer, requestPrice)
	if err != nil {
		t.Fatal(err)
	}
	err = debitActionNormal.Apply()
	if err != nil {
		t.Fatal(err)
	}
	debitActionNormal.Cleanup()

	// debit ghost balance
	debitActionGhost, err := acc.PrepareDebit(context.Background(), peer, requestPrice)
	if err != nil {
		t.Fatal(err)
	}
	debitActionGhost.Cleanup()

	// increase shadow reserve
	debitActionShadow, err := acc.PrepareDebit(context.Background(), peer, requestPrice)
	if err != nil {
		t.Fatal(err)
	}
	_ = debitActionShadow

	if blocklistTime != 0 {
		t.Fatal("unexpected blocklist")
	}

	acc.Disconnect(peer)

	if blocklistTime != int64(4*paymentThresholdInRefreshmentSeconds) {
		t.Fatalf("unexpected blocklisting time, got %v expected %v", blocklistTime, 4*paymentThresholdInRefreshmentSeconds)
	}

}

func TestAccountingResetBalanceAfterReconnect(t *testing.T) {
	logger := log.Noop

	store := mock.NewStateStore()
	defer store.Close()

	var blocklistTime int64

	paymentThresholdInRefreshmentSeconds := new(big.Int).Div(testPaymentThreshold, big.NewInt(testRefreshRate)).Uint64()

	f := func(s swarm.Address, t time.Duration, reason string) error {
		if reason != "disconnected" {
			return errInvalidReason
		}
		blocklistTime = int64(t.Seconds())
		return nil
	}

	acc, err := accounting.NewAccounting(testPaymentThreshold, testPaymentTolerance, testPaymentEarly, logger, store, nil, big.NewInt(testRefreshRate), p2pmock.New(p2pmock.WithBlocklistFunc(f)))
	if err != nil {
		t.Fatal(err)
	}

	ts := int64(1000)
	acc.SetTime(ts)

	peer, err := swarm.ParseHexAddress("00112233")
	if err != nil {
		t.Fatal(err)
	}
	acc.Connect(peer)

	requestPrice := testPaymentThreshold.Uint64()

	debitActionNormal, err := acc.PrepareDebit(context.Background(), peer, requestPrice)
	if err != nil {
		t.Fatal(err)
	}
	err = debitActionNormal.Apply()
	if err != nil {
		t.Fatal(err)
	}
	debitActionNormal.Cleanup()

	// debit ghost balance
	debitActionGhost, err := acc.PrepareDebit(context.Background(), peer, requestPrice)
	if err != nil {
		t.Fatal(err)
	}
	debitActionGhost.Cleanup()

	// increase shadow reserve
	debitActionShadow, err := acc.PrepareDebit(context.Background(), peer, requestPrice)
	if err != nil {
		t.Fatal(err)
	}
	_ = debitActionShadow

	if blocklistTime != 0 {
		t.Fatal("unexpected blocklist")
	}

	acc.Disconnect(peer)

	if blocklistTime != int64(4*paymentThresholdInRefreshmentSeconds) {
		t.Fatalf("unexpected blocklisting time, got %v expected %v", blocklistTime, 4*paymentThresholdInRefreshmentSeconds)
	}

	acc.Connect(peer)

	balance, err := acc.Balance(peer)
	if err != nil {
		t.Fatal(err)
	}

	if balance.Int64() != 0 {
		t.Fatalf("balance for peer %v not as expected got %d, wanted 0", peer.String(), balance)
	}

	surplusBalance, err := acc.SurplusBalance(peer)
	if err != nil {
		t.Fatal(err)
	}

	if surplusBalance.Int64() != 0 {
		t.Fatalf("surplus balance for peer %v not as expected got %d, wanted 0", peer.String(), balance)
	}

}
