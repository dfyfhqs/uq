package queue

import (
	"bytes"
	"encoding/gob"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/buaazp/uq/store"
)

const (
	StorageKeyWord     string        = "UnitedQueueKey"
	ErrTopicNotExisted string        = "Topic Not Existed"
	ErrLineNotExisted  string        = "Line Not Existed"
	ErrTopicExisted    string        = "Topic Has Existed"
	ErrLineExisted     string        = "Line Has Existed"
	ErrNotDelivered    string        = "Message Not Delivered"
	ErrKey             string        = "Key Illegal"
	ErrNone            string        = "No Message"
	BgBackupInterval   time.Duration = 10 * time.Second
	BgCleanInterval    time.Duration = 20 * time.Second
	BgCleanTimeout     time.Duration = 5 * time.Second
)

type UnitedQueue struct {
	mu      sync.Mutex
	topics  map[string]*topic
	storage store.Storage
}

type unitedQueueStore struct {
	Topics []string
}

func NewUnitedQueue(storage store.Storage) (*UnitedQueue, error) {
	topics := make(map[string]*topic)
	uq := new(UnitedQueue)
	uq.topics = topics
	uq.storage = storage

	err := uq.loadQueue()
	if err != nil {
		return nil, err
	}
	return uq, nil
}

func (u *UnitedQueue) loadQueue() error {
	unitedQueueStoreData, err := u.storage.Get(StorageKeyWord)
	if err != nil {
		log.Printf("storage not existed: %s", err)
		return nil
	}

	if len(unitedQueueStoreData) > 0 {
		var unitedQueueStoreValue unitedQueueStore
		dec := gob.NewDecoder(bytes.NewBuffer(unitedQueueStoreData))
		if e := dec.Decode(&unitedQueueStoreValue); e == nil {
			for _, topicName := range unitedQueueStoreValue.Topics {
				topicStoreData, err := u.storage.Get(topicName)
				if err != nil || len(topicStoreData) == 0 {
					continue
				}
				var topicStoreValue topicStore
				dec2 := gob.NewDecoder(bytes.NewBuffer(topicStoreData))
				if e := dec2.Decode(&topicStoreValue); e == nil {
					t, err := u.loadTopic(topicName, topicStoreValue)
					if err != nil {
						continue
					}
					u.topics[topicName] = t
				}
			}
		}
	}

	log.Printf("united queue load finisded.")
	log.Printf("u.topics: %v", u.topics)
	return nil
}

func (u *UnitedQueue) loadTopic(topicName string, topicStoreValue topicStore) (*topic, error) {
	t := new(topic)
	t.name = topicName
	t.head = topicStoreValue.Head
	t.tail = topicStoreValue.Tail
	t.q = u
	t.quit = make(chan bool)

	lines := make(map[string]*line)
	for _, lineName := range topicStoreValue.Lines {
		lineStoreKey := fmt.Sprintf("%s/%s", topicName, lineName)
		lineStoreData, err := u.storage.Get(lineStoreKey)
		if err != nil || len(lineStoreData) == 0 {
			continue
		}
		var lineStoreValue lineStore
		dec3 := gob.NewDecoder(bytes.NewBuffer(lineStoreData))
		if e := dec3.Decode(&lineStoreValue); e == nil {
			l, err := t.loadLine(lineName, lineStoreValue)
			if err != nil {
				continue
			}
			lines[lineName] = l
			log.Printf("line[%s] load succ.", lineStoreKey)
			log.Printf("line: %v", l)
		}
	}
	t.lines = lines

	t.start()
	log.Printf("topic[%s] load succ.", topicName)
	log.Printf("topic: %v", t)
	return t, nil
}

func (u *UnitedQueue) Create(cr *CreateRequest) error {
	u.mu.Lock()
	defer u.mu.Unlock()

	var err error
	if cr.LineName != "" {
		t, ok := u.topics[cr.TopicName]
		if !ok {
			return errors.New(ErrTopicNotExisted)
		}

		err = t.CreateLine(cr.LineName, cr.Recycle)
		if err != nil {
			log.Printf("create line[%s] error: %s", cr.LineName, err)
		}
	} else if cr.TopicName != "" {
		err = u.createTopic(cr.TopicName)
		if err != nil {
			log.Printf("create topic[%s] error: %s", cr.TopicName, err)
		}
	} else {
		return errors.New(ErrKey)
	}

	return err
}

func (u *UnitedQueue) createTopic(name string) error {
	_, ok := u.topics[name]
	if ok {
		return errors.New(ErrTopicExisted)
	}

	t, err := u.newTopic(name)
	if err != nil {
		return err
	}
	u.topics[name] = t

	err = u.exportQueue()
	if err != nil {
		delete(u.topics, name)
		return err
	}

	log.Printf("topic[%s] created.", name)
	return nil
}

func (u *UnitedQueue) newTopic(name string) (*topic, error) {
	lines := make(map[string]*line)
	t := new(topic)
	t.name = name
	t.lines = lines
	t.head = 0
	t.tail = 0
	t.q = u
	t.quit = make(chan bool)

	t.start()
	return t, nil
}

func (u *UnitedQueue) exportQueue() error {
	log.Printf("start export queue...")

	queueStoreValue, err := u.genQueueStore()
	if err != nil {
		return err
	}

	buffer := bytes.NewBuffer(nil)
	enc := gob.NewEncoder(buffer)
	err = enc.Encode(queueStoreValue)
	if err != nil {
		return err
	}

	err = u.storage.Set(StorageKeyWord, buffer.Bytes())
	if err != nil {
		return err
	}

	log.Printf("united queue export finisded.")
	return nil
}

func (u *UnitedQueue) genQueueStore() (*unitedQueueStore, error) {
	topics := make([]string, len(u.topics))
	i := 0
	for topicName, _ := range u.topics {
		topics[i] = topicName
		i++
	}

	qs := new(unitedQueueStore)
	qs.Topics = topics
	return qs, nil
}

func (u *UnitedQueue) Push(name string, data []byte) error {
	t, ok := u.topics[name]
	if !ok {
		return errors.New(ErrTopicNotExisted)
	}

	return t.Push(data)
}

func (u *UnitedQueue) Pop(name string) (uint64, []byte, error) {
	parts := strings.Split(name, "/")
	if len(parts) != 2 {
		return 0, nil, errors.New(ErrKey)
	}

	tName := parts[0]
	lName := parts[1]

	t, ok := u.topics[tName]
	if !ok {
		log.Printf("topic[%s] not existed.", tName)
		return 0, nil, errors.New(ErrTopicNotExisted)
	}

	return t.pop(lName)
}

func (u *UnitedQueue) Confirm(cr *ConfirmRequest) error {
	t, ok := u.topics[cr.TopicName]
	if !ok {
		log.Printf("topic[%s] not existed.", cr.TopicName)
		return errors.New(ErrTopicNotExisted)
	}

	return t.confirm(cr.LineName, cr.ID)
}

func (u *UnitedQueue) setData(key string, data []byte) error {
	return u.storage.Set(key, data)
	// err := u.storage.Set(key, data)
	// if err != nil {
	// 	log.Printf("key[%s] set data error: %s", key, err)
	// } else {
	// 	log.Printf("key[%s] set data succ", key)
	// }
	// return nil
}

func (u *UnitedQueue) getData(key string) ([]byte, error) {
	return u.storage.Get(key)
	// data, err := u.storage.Get(key)
	// if err != nil {
	// 	log.Printf("key[%s] get data error: %s", key, err)
	// 	return nil, err
	// }
	// return data, nil
}

func (u *UnitedQueue) delData(key string) error {
	return u.storage.Del(key)
	// err := u.storage.Del(key)
	// if err != nil {
	// 	log.Printf("key[%s] del data error: %s", key, err)
	// 	return err
	// }
	// return nil
}

func (u *UnitedQueue) Close() {
	u.mu.Lock()
	defer u.mu.Unlock()

	log.Printf("uq stoping...")
	for _, t := range u.topics {
		close(t.quit)
	}

	err := u.exportTopics()
	if err != nil {
		log.Printf("export queue error: %s", err)
	}

	u.storage.Close()
}

func (u *UnitedQueue) exportTopics() error {
	for _, t := range u.topics {
		err := t.exportLines()
		if err != nil {
			log.Printf("topic[%s] export lines error: %s", t.name, err)
			continue
		}
		err = t.exportTopic()
		if err != nil {
			log.Printf("topic[%s] export error: %s", t.name, err)
			continue
		}
	}

	log.Printf("export all topics succ.")
	return nil
}
