package main

import (
	"encoding/json"

	"github.com/nbd-wtf/go-nostr"
)

// BaseEntity contains common fields used across many entity types
type BaseEntity struct {
	Type string `json:"type,omitempty"`
	ID   string `json:"id,omitempty"`
	Name string `json:"name,omitempty"`
}

type ControlledVocabulary struct {
	Type       string `json:"type,omitempty"`
	ID         string `json:"id"`
	PrefLabel  string `json:"prefLabel"`
	InLanguage string `json:"inLanguage,omitempty"`
}

// LanguageEntity adds language support to entities
type LanguageEntity struct {
	InLanguage string `json:"inLanguage,omitempty"`
}

// LabeledEntity adds prefLabel to entities
type LabeledEntity struct {
	PrefLabel string `json:"prefLabel,omitempty"`
}

// About represents a topic or subject
type About struct {
	ControlledVocabulary
}

// Creator represents the creator of content
type Creator struct {
	BaseEntity
	Affiliation   *Affiliation `json:"affiliation,omitempty"`
	HonoricPrefix string       `json:"honoricPrefix,omitempty"`
}

// Contributor represents someone who contributed to the content
type Contributor struct {
	BaseEntity
	HonoricPrefix string `json:"honoricPrefix,omitempty"`
}

// Publisher represents the publisher of the content
type Publisher struct {
	BaseEntity
}

// Funder represents an entity that funded the content
type Funder struct {
	BaseEntity
}

// Affiliation represents an organization affiliation
type Affiliation struct {
	BaseEntity
}

// ConditionsOfAccess represents access conditions
type ConditionsOfAccess struct {
	ControlledVocabulary
}

// LearningResourceType categorizes the learning resource
type LearningResourceType struct {
	ControlledVocabulary
}

// Audience represents the target audience
type Audience struct {
	ControlledVocabulary
}

// Teaches represents what the content teaches
type Teaches struct {
	ID string `json:"id"`
	LabeledEntity
	LanguageEntity
}

// Assesses represents what the content assesses
type Assesses struct {
	ID string `json:"id"`
	LabeledEntity
	LanguageEntity
}

// CompetencyRequired represents required competencies
type CompetencyRequired struct {
	ID string `json:"id"`
	LabeledEntity
	LanguageEntity
}

// EducationalLevel represents the educational level
type EducationalLevel struct {
	ControlledVocabulary
}

// InteractivityType represents the type of interactivity
type InteractivityType struct {
	ControlledVocabulary
}

// IsBasedOn represents a reference to source material
type IsBasedOn struct {
	Type    string   `json:"type,omitempty"`
	Name    string   `json:"name"`
	Creator *Creator `json:"creator,omitempty"`
	License *License `json:"license,omitempty"`
}

// IsPartOf represents a parent relationship
type IsPartOf struct {
	BaseEntity
}

type HasPart struct {
	BaseEntity
}

// Trailer represents a media trailer
type Trailer struct {
	Type           string `json:"type"`
	ContentUrl     string `json:"contentUrl"`
	EncodingFormat string `json:"encodingFormat"`
	ContentSize    string `json:"contentSize,omitempty"`
	Sha256         string `json:"sha256,omitempty"`
	EmbedUrl       string `json:"embedUrl,omitempty"`
	Bitrate        string `json:"bitrate,omitempty"`
}

// License represents the content license
type License struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
}

// NostrMetadata contains Nostr-specific metadata
type NostrMetadata struct {
	EventID        string          `json:"eventID"`
	EventKind      int             `json:"eventKind"`
	EventPubKey    string          `json:"eventPubKey"`
	EventSig       string          `json:"eventSignature"`
	EventCreatedAt nostr.Timestamp `json:"eventCreatedAt"`
	EventContent   string          `json:"eventContent"`
	EventRaw       string          `json:"eventRaw"`
}

// AMBMetadata represents the full metadata structure
type AMBMetadata struct {
	// Document ID, same a d-tag
	ID          string     `json:"id"`
	D           string     `json:"d"`
	Type        string     `json:"type"`
	Name        string     `json:"name"`
	Description string     `json:"description,omitempty"`
	About       []*About   `json:"about,omitempty"`
	Keywords    []string   `json:"keywords,omitempty"`
	InLanguage  []string   `json:"inLanguage,omitempty"`
	Image       string     `json:"image,omitempty"`
	Trailer     []*Trailer `json:"trailer,omitempty"`

	// Provenience
	Creator       []*Creator     `json:"creator,omitempty"`
	Contributor   []*Contributor `json:"contributor,omitempty"`
	DateCreated   string         `json:"dateCreated,omitempty"`
	DatePublished string         `json:"datePublished,omitempty"`
	DateModified  string         `json:"dateModified,omitempty"`
	Publisher     []*Publisher   `json:"publisher,omitempty"`
	Funder        []*Funder      `json:"funder,omitempty"`

	// Costs and Rights
	IsAccessibleForFree bool                `json:"isAccessibleForFree,omitempty"`
	License             *License            `json:"license,omitempty"`
	ConditionsOfAccess  *ConditionsOfAccess `json:"conditionsOfAccess,omitempty"`

	// Educational metadata
	LearningResourceType []*LearningResourceType `json:"learningResourceType,omitempty"`
	Audience             []*Audience             `json:"audience,omitempty"`
	Teaches              []*Teaches              `json:"teaches,omitempty"`
	Assesses             []*Assesses             `json:"assesses,omitempty"`
	CompetencyRequired   []*CompetencyRequired   `json:"competencyRequired,omitempty"`
	EducationalLevel     []*EducationalLevel     `json:"educationalLevel,omitempty"`
	InteractivityType    *InteractivityType      `json:"interactivityType,omitempty"`

	// Relation
	IsBasedOn []*IsBasedOn `json:"isBasedOn,omitempty"`
	IsPartOf  []*IsPartOf  `json:"isPartOf,omitempty"`
	HasPart   []*HasPart   `json:"hasPart,omitempty"`

	// Technical
	Duration string `json:"duration,omitempty"`
	// TODO Encoding  ``
	// TODO Caption

	// Nostr integration
	NostrMetadata `json:",inline"`
}

// converts a nostr event to stringified JSON
func eventToStringifiedJSON(event *nostr.Event) (string, error) {
	jsonData, err := json.Marshal(event)
	if err != nil {
		return "", err
	}

	jsonString := string(jsonData)
	return jsonString, err
}

// NostrToAMB converts a Nostr event of kind 30142 to AMB metadata
func NostrToAMB(event *nostr.Event) (*AMBMetadata, error) {
	eventRaw, _ := eventToStringifiedJSON(event)

	amb := &AMBMetadata{
		Type: "LearningResource",
		NostrMetadata: NostrMetadata{
			EventID:        event.ID,
			EventPubKey:    event.PubKey,
			EventContent:   event.Content,
			EventCreatedAt: event.CreatedAt,
			EventKind:      event.Kind,
			EventSig:       event.Sig,
			EventRaw:       eventRaw,
		},
	}

	for _, tag := range event.Tags {
		if len(tag) < 2 {
			continue
		}

		// TODO alle Attribute durchgehen für das parsen
		switch tag[0] {
		case "d":
			if len(tag) >= 2 {
				amb.ID = tag[1]
        amb.D = tag[1]
			}
		case "name":
			if len(tag) >= 2 {
				amb.Name = tag[1]
			}
		case "description":
			if len(tag) >= 2 {
				amb.Description = tag[1]
			}
		case "creator":
			if len(tag) >= 2 {
				creator := &Creator{}
				creator.Name = tag[1]
				if len(tag) >= 3 {
					creator.ID = tag[2]
				}
				if len(tag) >= 4 {
					creator.Type = tag[3]
				}

				amb.Creator = append(amb.Creator, creator)
			}
		case "image":
			if len(tag) >= 2 {
				amb.Image = tag[1]
			}
		case "about":
			if len(tag) >= 3 {
				subject := &About{}
				subject.PrefLabel = tag[1]
				subject.InLanguage = tag[2]
				if len(tag) >= 4 {
					subject.ID = tag[3]
				}
				amb.About = append(amb.About, subject)
			}
		case "learningResourceType":
			if len(tag) >= 3 {
				lrt := &LearningResourceType{}
				lrt.PrefLabel = tag[1]
				lrt.InLanguage = tag[2]
				if len(tag) >= 4 {
					lrt.ID = tag[3]
				}
				amb.LearningResourceType = append(amb.LearningResourceType, lrt)
			}
		case "inLanguage":
			if len(tag) >= 2 {
				amb.InLanguage = append(amb.InLanguage, tag[1])
			}
		case "keywords":
			if len(tag) >= 2 {
				amb.Keywords = tag[1:]
			}
		case "license":
			if len(tag) >= 3 {
				amb.License = &License{}
				amb.License.ID = tag[1]
				amb.License.Name = tag[2]

			}
		case "datePublished":
			if len(tag) >= 2 {
				amb.DatePublished = tag[1]
			}
		}
	}

	return amb, nil
}

// converts a stringified JSON event to a nostr.Event
func StringifiedJSONToNostrEvent(jsonString string) (nostr.Event, error) {
	var event nostr.Event
	err := json.Unmarshal([]byte(jsonString), &event)
	if err != nil {
		return nostr.Event{}, err
	}
	return event, nil
}
