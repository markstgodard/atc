package db

import (
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/concourse/atc"
)

//go:generate counterfeiter . TeamDB

type TeamDB interface {
	GetTeam() (SavedTeam, bool, error)
	GetConfig(pipelineName string) (atc.Config, atc.RawConfig, ConfigVersion, error)
}

type teamDB struct {
	teamName string

	conn         Conn
	buildFactory *buildFactory
}

func (db *teamDB) GetConfig(pipelineName string) (atc.Config, atc.RawConfig, ConfigVersion, error) {
	var configBlob []byte
	var version int
	err := db.conn.QueryRow(`
		SELECT config, version
		FROM pipelines
		WHERE name = $1 AND team_id = (
			SELECT id
			FROM teams
			WHERE LOWER(name) = LOWER($2)
		)
	`, pipelineName, db.teamName).Scan(&configBlob, &version)
	if err != nil {
		if err == sql.ErrNoRows {
			return atc.Config{}, atc.RawConfig(""), 0, nil
		}
		return atc.Config{}, atc.RawConfig(""), 0, err
	}

	var config atc.Config
	err = json.Unmarshal(configBlob, &config)
	if err != nil {
		return atc.Config{}, atc.RawConfig(string(configBlob)), ConfigVersion(version), atc.MalformedConfigError{err}
	}

	return config, atc.RawConfig(string(configBlob)), ConfigVersion(version), nil
}

func (db *teamDB) registerSerialGroup(tx Tx, jobName, serialGroup string, pipelineID int) error {
	_, err := tx.Exec(`
    INSERT INTO jobs_serial_groups (serial_group, job_id) VALUES
    ($1, (SELECT j.id
                  FROM jobs j
                       JOIN pipelines p
                         ON j.pipeline_id = p.id
                  WHERE j.name = $2
                    AND j.pipeline_id = $3
                 LIMIT  1));`,
		serialGroup, jobName, pipelineID,
	)

	return swallowUniqueViolation(err)
}

func (db *teamDB) GetTeam() (SavedTeam, bool, error) {
	query := `
		SELECT id, name, admin
		FROM teams
		WHERE LOWER(name) = LOWER($1)
	`
	params := []interface{}{db.teamName}
	savedTeam, err := db.queryTeam(query, params)
	if err != nil {
		if err == sql.ErrNoRows {
			return savedTeam, false, nil
		}

		return savedTeam, false, err
	}

	return savedTeam, true, nil
}

func (db *teamDB) queryTeam(query string, params []interface{}) (SavedTeam, error) {
	var savedTeam SavedTeam

	tx, err := db.conn.Begin()
	if err != nil {
		return SavedTeam{}, err
	}
	defer tx.Rollback()

	err = tx.QueryRow(query, params...).Scan(
		&savedTeam.ID,
		&savedTeam.Name,
		&savedTeam.Admin,
	)
	if err != nil {
		return savedTeam, err
	}
	err = tx.Commit()
	if err != nil {
		return savedTeam, err
	}

	return savedTeam, nil
}

func scanPipeline(rows scannable) (SavedPipeline, error) {
	var id int
	var name string
	var configBlob []byte
	var version int
	var paused bool
	var public bool
	var teamID int
	var teamName string

	err := rows.Scan(&id, &name, &configBlob, &version, &paused, &teamID, &public, &teamName)
	if err != nil {
		return SavedPipeline{}, err
	}

	var config atc.Config
	err = json.Unmarshal(configBlob, &config)
	if err != nil {
		return SavedPipeline{}, err
	}

	return SavedPipeline{
		ID:       id,
		Paused:   paused,
		Public:   public,
		TeamID:   teamID,
		TeamName: teamName,
		Pipeline: Pipeline{
			Name:    name,
			Config:  config,
			Version: ConfigVersion(version),
		},
	}, nil
}

func scanPipelines(rows *sql.Rows) ([]SavedPipeline, error) {
	pipelines := []SavedPipeline{}

	for rows.Next() {
		pipeline, err := scanPipeline(rows)
		if err != nil {
			return nil, err
		}

		pipelines = append(pipelines, pipeline)
	}

	return pipelines, nil
}

type PipelinePausedState string

const (
	PipelinePaused   PipelinePausedState = "paused"
	PipelineUnpaused PipelinePausedState = "unpaused"
	PipelineNoChange PipelinePausedState = "nochange"
)

func (state PipelinePausedState) Bool() *bool {
	yes := true
	no := false

	switch state {
	case PipelinePaused:
		return &yes
	case PipelineUnpaused:
		return &no
	case PipelineNoChange:
		return nil
	default:
		panic("unknown pipeline state")
	}
}

func mapHash(m map[string]interface{}) string {
	j, _ := json.Marshal(m)
	return fmt.Sprintf("%x", sha256.Sum256(j))
}
