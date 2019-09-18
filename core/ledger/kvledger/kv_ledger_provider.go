/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package kvledger

import (
	"bytes"
	"fmt"

	"github.com/golang/protobuf/proto"
	"github.com/hyperledger/fabric-protos-go/common"
	"github.com/hyperledger/fabric/common/ledger/util/leveldbhelper"
	"github.com/hyperledger/fabric/core/ledger"
	"github.com/hyperledger/fabric/core/ledger/confighistory"
	"github.com/hyperledger/fabric/core/ledger/kvledger/bookkeeping"
	"github.com/hyperledger/fabric/core/ledger/kvledger/history"
	"github.com/hyperledger/fabric/core/ledger/kvledger/msgs"
	"github.com/hyperledger/fabric/core/ledger/kvledger/txmgmt/privacyenabledstate"
	"github.com/hyperledger/fabric/core/ledger/ledgerstorage"
	"github.com/hyperledger/fabric/core/ledger/pvtdatastorage"
	"github.com/hyperledger/fabric/protoutil"
	"github.com/pkg/errors"
	"github.com/syndtr/goleveldb/leveldb"
)

const idStoreFormatVersion = "2.0"

var (
	// ErrLedgerIDExists is thrown by a CreateLedger call if a ledger with the given id already exists
	ErrLedgerIDExists = errors.New("LedgerID already exists")
	// ErrNonExistingLedgerID is thrown by an OpenLedger call if a ledger with the given id does not exist
	ErrNonExistingLedgerID = errors.New("LedgerID does not exist")
	// ErrLedgerNotOpened is thrown by a CloseLedger call if a ledger with the given id has not been opened
	ErrLedgerNotOpened = errors.New("ledger is not opened yet")
	// ErrInactiveLedger is thrown by an OpenLedger call if a ledger with the given id is not active
	ErrInactiveLedger = errors.New("Ledger is not active")

	underConstructionLedgerKey = []byte("underConstructionLedgerKey")
	// ledgerKeyPrefix is the prefix for each ledger key in idStore db
	ledgerKeyPrefix = []byte{'l'}
	// ledgerKeyStop is the end key when querying idStore db by ledger key
	ledgerKeyStop = []byte{'l' + 1}
	// metadataKeyPrefix is the prefix for each metadata key in idStore db
	metadataKeyPrefix = []byte{'s'}
	// metadataKeyStop is the end key when querying idStore db by metadata key
	metadataKeyStop = []byte{'s' + 1}

	// formatKey
	formatKey = []byte("f")
)

// Provider implements interface ledger.PeerLedgerProvider
type Provider struct {
	idStore             *idStore
	ledgerStoreProvider *ledgerstorage.Provider
	vdbProvider         privacyenabledstate.DBProvider
	historydbProvider   *history.DBProvider
	configHistoryMgr    confighistory.Mgr
	stateListeners      []ledger.StateListener
	bookkeepingProvider bookkeeping.Provider
	initializer         *ledger.Initializer
	collElgNotifier     *collElgNotifier
	stats               *stats
	fileLock            *leveldbhelper.FileLock
	hasher              ledger.Hasher
}

// NewProvider instantiates a new Provider.
// This is not thread-safe and assumed to be synchronized by the caller
func NewProvider(initializer *ledger.Initializer) (pr *Provider, e error) {
	p := &Provider{
		initializer: initializer,
		hasher:      initializer.Hasher,
	}

	defer func() {
		if e != nil {
			p.Close()
		}
	}()

	fileLockPath := fileLockPath(initializer.Config.RootFSPath)
	fileLock := leveldbhelper.NewFileLock(fileLockPath)
	if err := fileLock.Lock(); err != nil {
		return nil, errors.Wrap(err, "as another peer node command is executing,"+
			" wait for that command to complete its execution or terminate it before retrying")
	}

	p.fileLock = fileLock
	// initialize the ID store (inventory of chainIds/ledgerIds)
	idStore, err := openIDStore(LedgerProviderPath(p.initializer.Config.RootFSPath))
	if err != nil {
		return nil, err
	}
	p.idStore = idStore
	// initialize ledger storage
	privateData := &pvtdatastorage.PrivateDataConfig{
		PrivateDataConfig: initializer.Config.PrivateDataConfig,
		StorePath:         PvtDataStorePath(p.initializer.Config.RootFSPath),
	}

	ledgerStoreProvider, err := ledgerstorage.NewProvider(
		BlockStorePath(p.initializer.Config.RootFSPath),
		privateData,
		p.initializer.MetricsProvider,
	)
	if err != nil {
		return nil, err
	}
	p.ledgerStoreProvider = ledgerStoreProvider
	if initializer.Config.HistoryDBConfig.Enabled {
		// Initialize the history database (index for history of values by key)
		historydbProvider, err := history.NewDBProvider(
			HistoryDBPath(p.initializer.Config.RootFSPath),
		)
		if err != nil {
			return nil, err
		}
		p.historydbProvider = historydbProvider
	}
	// initialize config history for chaincode
	configHistoryMgr, err := confighistory.NewMgr(
		ConfigHistoryDBPath(p.initializer.Config.RootFSPath),
		initializer.DeployedChaincodeInfoProvider,
	)
	if err != nil {
		return nil, err
	}
	p.configHistoryMgr = configHistoryMgr
	// initialize the collection eligibility notifier

	collElgNotifier := &collElgNotifier{
		initializer.DeployedChaincodeInfoProvider,
		initializer.MembershipInfoProvider,
		make(map[string]collElgListener),
	}
	p.collElgNotifier = collElgNotifier
	// initialize the state listeners
	stateListeners := initializer.StateListeners
	stateListeners = append(stateListeners, collElgNotifier)
	stateListeners = append(stateListeners, configHistoryMgr)
	p.stateListeners = stateListeners

	p.bookkeepingProvider, err = bookkeeping.NewProvider(
		BookkeeperDBPath(p.initializer.Config.RootFSPath),
	)
	if err != nil {
		return nil, err
	}
	stateDB := &privacyenabledstate.StateDBConfig{
		StateDBConfig: initializer.Config.StateDBConfig,
		LevelDBPath:   StateDBPath(p.initializer.Config.RootFSPath),
	}
	p.vdbProvider, err = privacyenabledstate.NewCommonStorageDBProvider(
		p.bookkeepingProvider,
		initializer.MetricsProvider,
		initializer.HealthCheckRegistry,
		stateDB,
	)
	if err != nil {
		return nil, err
	}
	p.stats = newStats(initializer.MetricsProvider)
	p.recoverUnderConstructionLedger()
	return p, nil
}

// Create implements the corresponding method from interface ledger.PeerLedgerProvider
// This functions sets a under construction flag before doing any thing related to ledger creation and
// upon a successful ledger creation with the committed genesis block, removes the flag and add entry into
// created ledgers list (atomically). If a crash happens in between, the 'recoverUnderConstructionLedger'
// function is invoked before declaring the provider to be usable
func (p *Provider) Create(genesisBlock *common.Block) (ledger.PeerLedger, error) {
	ledgerID, err := protoutil.GetChainIDFromBlock(genesisBlock)
	if err != nil {
		return nil, err
	}
	exists, err := p.idStore.ledgerIDExists(ledgerID)
	if err != nil {
		return nil, err
	}
	if exists {
		return nil, ErrLedgerIDExists
	}
	if err = p.idStore.setUnderConstructionFlag(ledgerID); err != nil {
		return nil, err
	}
	lgr, err := p.openInternal(ledgerID)
	if err != nil {
		logger.Errorf("Error opening a new empty ledger. Unsetting under construction flag. Error: %+v", err)
		panicOnErr(p.runCleanup(ledgerID), "Error running cleanup for ledger id [%s]", ledgerID)
		panicOnErr(p.idStore.unsetUnderConstructionFlag(), "Error while unsetting under construction flag")
		return nil, err
	}
	if err := lgr.CommitLegacy(&ledger.BlockAndPvtData{Block: genesisBlock}, &ledger.CommitOptions{}); err != nil {
		lgr.Close()
		return nil, err
	}
	panicOnErr(p.idStore.createLedgerID(ledgerID, genesisBlock), "Error while marking ledger as created")
	return lgr, nil
}

// Open implements the corresponding method from interface ledger.PeerLedgerProvider
func (p *Provider) Open(ledgerID string) (ledger.PeerLedger, error) {
	logger.Debugf("Open() opening kvledger: %s", ledgerID)
	// Check the ID store to ensure that the chainId/ledgerId exists
	active, exists, err := p.idStore.ledgerIDActive(ledgerID)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, ErrNonExistingLedgerID
	}
	if !active {
		return nil, ErrInactiveLedger
	}
	return p.openInternal(ledgerID)
}

func (p *Provider) openInternal(ledgerID string) (ledger.PeerLedger, error) {
	// Get the block store for a chain/ledger
	blockStore, err := p.ledgerStoreProvider.Open(ledgerID)
	if err != nil {
		return nil, err
	}
	p.collElgNotifier.registerListener(ledgerID, blockStore)

	// Get the versioned database (state database) for a chain/ledger
	vDB, err := p.vdbProvider.GetDBHandle(ledgerID)
	if err != nil {
		return nil, err
	}

	// Get the history database (index for history of values by key) for a chain/ledger
	var historyDB *history.DB
	if p.historydbProvider != nil {
		historyDB, err = p.historydbProvider.GetDBHandle(ledgerID)
		if err != nil {
			return nil, err
		}
	}

	// Create a kvLedger for this chain/ledger, which encasulates the underlying data stores
	// (id store, blockstore, state database, history database)
	l, err := newKVLedger(
		ledgerID,
		blockStore,
		vDB,
		historyDB,
		p.configHistoryMgr,
		p.stateListeners,
		p.bookkeepingProvider,
		p.initializer.DeployedChaincodeInfoProvider,
		p.initializer.ChaincodeLifecycleEventProvider,
		p.stats.ledgerStats(ledgerID),
		p.initializer.CustomTxProcessors,
		p.hasher,
	)
	if err != nil {
		return nil, err
	}
	return l, nil
}

// Exists implements the corresponding method from interface ledger.PeerLedgerProvider
func (p *Provider) Exists(ledgerID string) (bool, error) {
	return p.idStore.ledgerIDExists(ledgerID)
}

// List implements the corresponding method from interface ledger.PeerLedgerProvider
func (p *Provider) List() ([]string, error) {
	return p.idStore.getActiveLedgerIDs()
}

// Close implements the corresponding method from interface ledger.PeerLedgerProvider
func (p *Provider) Close() {
	if p.idStore != nil {
		p.idStore.close()
	}
	if p.ledgerStoreProvider != nil {
		p.ledgerStoreProvider.Close()
	}
	if p.vdbProvider != nil {
		p.vdbProvider.Close()
	}
	if p.bookkeepingProvider != nil {
		p.bookkeepingProvider.Close()
	}
	if p.configHistoryMgr != nil {
		p.configHistoryMgr.Close()
	}
	if p.historydbProvider != nil {
		p.historydbProvider.Close()
	}
	if p.fileLock != nil {
		p.fileLock.Unlock()
	}
}

// recoverUnderConstructionLedger checks whether the under construction flag is set - this would be the case
// if a crash had happened during creation of ledger and the ledger creation could have been left in intermediate
// state. Recovery checks if the ledger was created and the genesis block was committed successfully then it completes
// the last step of adding the ledger id to the list of created ledgers. Else, it clears the under construction flag
func (p *Provider) recoverUnderConstructionLedger() {
	logger.Debugf("Recovering under construction ledger")
	ledgerID, err := p.idStore.getUnderConstructionFlag()
	panicOnErr(err, "Error while checking whether the under construction flag is set")
	if ledgerID == "" {
		logger.Debugf("No under construction ledger found. Quitting recovery")
		return
	}
	logger.Infof("ledger [%s] found as under construction", ledgerID)
	ledger, err := p.openInternal(ledgerID)
	panicOnErr(err, "Error while opening under construction ledger [%s]", ledgerID)
	bcInfo, err := ledger.GetBlockchainInfo()
	panicOnErr(err, "Error while getting blockchain info for the under construction ledger [%s]", ledgerID)
	ledger.Close()

	switch bcInfo.Height {
	case 0:
		logger.Infof("Genesis block was not committed. Hence, the peer ledger not created. unsetting the under construction flag")
		panicOnErr(p.runCleanup(ledgerID), "Error while running cleanup for ledger id [%s]", ledgerID)
		panicOnErr(p.idStore.unsetUnderConstructionFlag(), "Error while unsetting under construction flag")
	case 1:
		logger.Infof("Genesis block was committed. Hence, marking the peer ledger as created")
		genesisBlock, err := ledger.GetBlockByNumber(0)
		panicOnErr(err, "Error while retrieving genesis block from blockchain for ledger [%s]", ledgerID)
		panicOnErr(p.idStore.createLedgerID(ledgerID, genesisBlock), "Error while adding ledgerID [%s] to created list", ledgerID)
	default:
		panic(errors.Errorf(
			"data inconsistency: under construction flag is set for ledger [%s] while the height of the blockchain is [%d]",
			ledgerID, bcInfo.Height))
	}
	return
}

// runCleanup cleans up blockstorage, statedb, and historydb for what
// may have got created during in-complete ledger creation
func (p *Provider) runCleanup(ledgerID string) error {
	// TODO - though, not having this is harmless for kv ledger.
	// If we want, following could be done:
	// - blockstorage could remove empty folders
	// - couchdb backed statedb could delete the database if got created
	// - leveldb backed statedb and history db need not perform anything as it uses a single db shared across ledgers
	return nil
}

func panicOnErr(err error, mgsFormat string, args ...interface{}) {
	if err == nil {
		return
	}
	args = append(args, err)
	panic(fmt.Sprintf(mgsFormat+" Error: %s", args...))
}

//////////////////////////////////////////////////////////////////////
// Ledger id persistence related code
///////////////////////////////////////////////////////////////////////
type idStore struct {
	db     *leveldbhelper.DB
	dbPath string
}

func openIDStore(path string) (s *idStore, e error) {
	db := leveldbhelper.CreateDB(&leveldbhelper.Conf{DBPath: path})
	db.Open()
	defer func() {
		if e != nil {
			db.Close()
		}
	}()

	emptyDB, err := db.IsEmpty()
	if err != nil {
		return nil, err
	}

	if emptyDB {
		// add format key to a new db
		err := db.Put(formatKey, []byte(idStoreFormatVersion), true)
		if err != nil {
			return nil, err
		}
		return &idStore{db, path}, nil
	}

	// verify the format is current for an existing db
	formatVersion, err := db.Get(formatKey)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(formatVersion, []byte(idStoreFormatVersion)) {
		return nil, &leveldbhelper.ErrFormatVersionMismatch{ExpectedFormatVersion: idStoreFormatVersion, DataFormatVersion: string(formatVersion), DBPath: path}
	}
	return &idStore{db, path}, nil
}

func (s *idStore) upgradeFormat() error {
	format, err := s.db.Get(formatKey)
	if err != nil {
		return err
	}
	idStoreFormatBytes := []byte(idStoreFormatVersion)
	if bytes.Equal(format, idStoreFormatBytes) {
		logger.Debug("Format is current, nothing to do")
		return nil
	}
	if format != nil {
		err = &leveldbhelper.ErrFormatVersionMismatch{ExpectedFormatVersion: "", DataFormatVersion: string(format), DBPath: s.dbPath}
		logger.Errorf("Failed to upgrade format [%#v] to new format [%#v]: %s", format, idStoreFormatBytes, err)
		return err
	}

	logger.Infof("The ledgerProvider db format is old, upgrading to the new format %s", idStoreFormatVersion)

	batch := &leveldb.Batch{}
	batch.Put(formatKey, idStoreFormatBytes)

	// add new metadata key for each ledger (channel)
	metadata, err := protoutil.Marshal(&msgs.LedgerMetadata{Status: msgs.Status_ACTIVE})
	if err != nil {
		logger.Errorf("Error marshalling ledger metadata: %s", err)
		return errors.Wrapf(err, "error marshalling ledger metadata")
	}
	itr := s.db.GetIterator(ledgerKeyPrefix, ledgerKeyStop)
	defer itr.Release()
	for itr.Error() == nil && itr.Next() {
		id := s.decodeLedgerID(itr.Key(), ledgerKeyPrefix)
		batch.Put(s.encodeLedgerKey(id, metadataKeyPrefix), metadata)
	}
	if err = itr.Error(); err != nil {
		logger.Errorf("Error while upgrading idStore format: %s", err)
		return errors.Wrapf(err, "error while upgrading idStore format")
	}

	return s.db.WriteBatch(batch, true)
}

func (s *idStore) setUnderConstructionFlag(ledgerID string) error {
	return s.db.Put(underConstructionLedgerKey, []byte(ledgerID), true)
}

func (s *idStore) unsetUnderConstructionFlag() error {
	return s.db.Delete(underConstructionLedgerKey, true)
}

func (s *idStore) getUnderConstructionFlag() (string, error) {
	val, err := s.db.Get(underConstructionLedgerKey)
	if err != nil {
		return "", err
	}
	return string(val), nil
}

func (s *idStore) createLedgerID(ledgerID string, gb *common.Block) error {
	gbKey := s.encodeLedgerKey(ledgerID, ledgerKeyPrefix)
	metadataKey := s.encodeLedgerKey(ledgerID, metadataKeyPrefix)
	var val []byte
	var metadata []byte
	var err error
	if val, err = s.db.Get(gbKey); err != nil {
		return err
	}
	if val != nil {
		return ErrLedgerIDExists
	}
	if val, err = proto.Marshal(gb); err != nil {
		return err
	}
	if metadata, err = protoutil.Marshal(&msgs.LedgerMetadata{Status: msgs.Status_ACTIVE}); err != nil {
		return err
	}
	batch := &leveldb.Batch{}
	batch.Put(gbKey, val)
	batch.Put(metadataKey, metadata)
	batch.Delete(underConstructionLedgerKey)
	return s.db.WriteBatch(batch, true)
}

func (s *idStore) updateLedgerStatus(ledgerID string, newStatus msgs.Status) error {
	metadata, err := s.getLedgerMetadata(ledgerID)
	if err != nil {
		return err
	}
	if metadata == nil {
		logger.Errorf("LedgerID [%s] does not exist", ledgerID)
		return ErrNonExistingLedgerID
	}
	if metadata.Status == newStatus {
		logger.Infof("Ledger [%s] is already in [%s] status, nothing to do", ledgerID, newStatus)
		return nil
	}
	metadata.Status = newStatus
	metadataBytes, err := proto.Marshal(metadata)
	if err != nil {
		logger.Errorf("Error marshalling ledger metadata: %s", err)
		return errors.Wrapf(err, "error marshalling ledger metadata")
	}
	logger.Infof("Updating ledger [%s] status to [%s]", ledgerID, newStatus)
	key := s.encodeLedgerKey(ledgerID, metadataKeyPrefix)
	return s.db.Put(key, metadataBytes, true)
}

func (s *idStore) getLedgerMetadata(ledgerID string) (*msgs.LedgerMetadata, error) {
	val, err := s.db.Get(s.encodeLedgerKey(ledgerID, metadataKeyPrefix))
	if val == nil || err != nil {
		return nil, err
	}
	metadata := &msgs.LedgerMetadata{}
	if err := proto.Unmarshal(val, metadata); err != nil {
		logger.Errorf("Error unmarshalling ledger metadata: %s", err)
		return nil, errors.Wrapf(err, "error unmarshalling ledger metadata")
	}
	return metadata, nil
}

func (s *idStore) ledgerIDExists(ledgerID string) (bool, error) {
	key := s.encodeLedgerKey(ledgerID, ledgerKeyPrefix)
	val := []byte{}
	err := error(nil)
	if val, err = s.db.Get(key); err != nil {
		return false, err
	}
	return val != nil, nil
}

// ledgerIDActive returns if a ledger is active and existed
func (s *idStore) ledgerIDActive(ledgerID string) (bool, bool, error) {
	metadata, err := s.getLedgerMetadata(ledgerID)
	if metadata == nil || err != nil {
		return false, false, err
	}
	return metadata.Status == msgs.Status_ACTIVE, true, nil
}

func (s *idStore) getActiveLedgerIDs() ([]string, error) {
	var ids []string
	itr := s.db.GetIterator(metadataKeyPrefix, metadataKeyStop)
	defer itr.Release()
	for itr.Error() == nil && itr.Next() {
		metadata := &msgs.LedgerMetadata{}
		if err := proto.Unmarshal(itr.Value(), metadata); err != nil {
			logger.Errorf("Error unmarshalling ledger metadata: %s", err)
			return nil, errors.Wrapf(err, "error unmarshalling ledger metadata")
		}
		if metadata.Status == msgs.Status_ACTIVE {
			id := s.decodeLedgerID(itr.Key(), metadataKeyPrefix)
			ids = append(ids, id)
		}
	}
	if err := itr.Error(); err != nil {
		logger.Errorf("Error getting ledger ids from idStore: %s", err)
		return nil, errors.Wrapf(err, "error getting ledger ids from idStore")
	}
	return ids, nil
}

func (s *idStore) close() {
	s.db.Close()
}

func (s *idStore) encodeLedgerKey(ledgerID string, prefix []byte) []byte {
	return append(prefix, []byte(ledgerID)...)
}

func (s *idStore) decodeLedgerID(key []byte, prefix []byte) string {
	return string(key[len(prefix):])
}
