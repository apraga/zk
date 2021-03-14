package sqlite

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/mickael-menu/zk/core"
	"github.com/mickael-menu/zk/core/note"
	"github.com/mickael-menu/zk/util"
	"github.com/mickael-menu/zk/util/errors"
	"github.com/mickael-menu/zk/util/fts5"
	"github.com/mickael-menu/zk/util/icu"
	"github.com/mickael-menu/zk/util/opt"
	"github.com/mickael-menu/zk/util/paths"
	strutil "github.com/mickael-menu/zk/util/strings"
)

// NoteDAO persists notes in the SQLite database.
// It implements the core port note.Finder.
type NoteDAO struct {
	tx     Transaction
	logger util.Logger

	// Prepared SQL statements
	indexedStmt            *LazyStmt
	addStmt                *LazyStmt
	updateStmt             *LazyStmt
	removeStmt             *LazyStmt
	findIdByPathStmt       *LazyStmt
	findIdByPathPrefixStmt *LazyStmt
	addLinkStmt            *LazyStmt
	setLinksTargetStmt     *LazyStmt
	removeLinksStmt        *LazyStmt
}

// NewNoteDAO creates a new instance of a DAO working on the given database
// transaction.
func NewNoteDAO(tx Transaction, logger util.Logger) *NoteDAO {
	return &NoteDAO{
		tx:     tx,
		logger: logger,

		// Get file info about all indexed notes.
		indexedStmt: tx.PrepareLazy(`
			SELECT path, modified from notes
			 ORDER BY sortable_path ASC
		`),

		// Add a new note to the index.
		addStmt: tx.PrepareLazy(`
			INSERT INTO notes (path, sortable_path, title, lead, body, raw_content, word_count, metadata, checksum, created, modified)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		`),

		// Update the content of a note.
		updateStmt: tx.PrepareLazy(`
			UPDATE notes
			   SET title = ?, lead = ?, body = ?, raw_content = ?, word_count = ?, metadata = ?, checksum = ?, modified = ?
			 WHERE path = ?
		`),

		// Remove a note.
		removeStmt: tx.PrepareLazy(`
			DELETE FROM notes
			 WHERE id = ?
		`),

		// Find a note ID from its exact path.
		findIdByPathStmt: tx.PrepareLazy(`
			SELECT id FROM notes
			 WHERE path = ?
		`),

		// Find a note ID from a prefix of its path.
		findIdByPathPrefixStmt: tx.PrepareLazy(`
			SELECT id FROM notes
			 WHERE path LIKE ? || '%'
		`),

		// Add a new link.
		addLinkStmt: tx.PrepareLazy(`
			INSERT INTO links (source_id, target_id, title, href, external, rels, snippet, snippet_start, snippet_end)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		`),

		// Set links matching a given href and missing a target ID to the given
		// target ID.
		setLinksTargetStmt: tx.PrepareLazy(`
			UPDATE links
			   SET target_id = ?
			 WHERE target_id IS NULL AND external = 0 AND ? LIKE href || '%'
		`),

		// Remove all the outbound links of a note.
		removeLinksStmt: tx.PrepareLazy(`
			DELETE FROM links
			 WHERE source_id = ?
		`),
	}
}

// Indexed returns file info of all indexed notes.
func (d *NoteDAO) Indexed() (<-chan paths.Metadata, error) {
	rows, err := d.indexedStmt.Query()
	if err != nil {
		return nil, err
	}

	c := make(chan paths.Metadata)
	go func() {
		defer close(c)
		defer rows.Close()
		var (
			path     string
			modified time.Time
		)

		for rows.Next() {
			err := rows.Scan(&path, &modified)
			if err != nil {
				d.logger.Err(err)
			}

			c <- paths.Metadata{
				Path:     path,
				Modified: modified,
			}
		}

		err = rows.Err()
		if err != nil {
			d.logger.Err(err)
		}
	}()

	return c, nil
}

// Add inserts a new note to the index.
func (d *NoteDAO) Add(note note.Metadata) (core.NoteId, error) {
	// For sortable_path, we replace in path / by the shortest non printable
	// character available to make it sortable. Without this, sorting by the
	// path would be a lexicographical sort instead of being the same order
	// returned by filepath.Walk.
	// \x01 is used instead of \x00, because SQLite treats \x00 as and end of
	// string.
	sortablePath := strings.ReplaceAll(note.Path, "/", "\x01")

	metadata, err := d.metadataToJson(note)
	if err != nil {
		return 0, err
	}

	res, err := d.addStmt.Exec(
		note.Path, sortablePath, note.Title, note.Lead, note.Body,
		note.RawContent, note.WordCount, metadata, note.Checksum, note.Created,
		note.Modified,
	)
	if err != nil {
		return 0, err
	}

	lastId, err := res.LastInsertId()
	if err != nil {
		return core.NoteId(0), err
	}

	id := core.NoteId(lastId)
	err = d.addLinks(id, note)
	return id, err
}

// Update modifies an existing note.
func (d *NoteDAO) Update(note note.Metadata) (core.NoteId, error) {
	id, err := d.findIdByPath(note.Path)
	if err != nil {
		return 0, err
	}
	if !id.IsValid() {
		return 0, errors.New("note not found in the index")
	}

	metadata, err := d.metadataToJson(note)
	if err != nil {
		return 0, err
	}

	_, err = d.updateStmt.Exec(
		note.Title, note.Lead, note.Body, note.RawContent, note.WordCount,
		metadata, note.Checksum, note.Modified, note.Path,
	)
	if err != nil {
		return id, err
	}

	_, err = d.removeLinksStmt.Exec(d.idToSql(id))
	if err != nil {
		return id, err
	}

	err = d.addLinks(id, note)
	return id, err
}

func (d *NoteDAO) metadataToJson(note note.Metadata) (string, error) {
	json, err := json.Marshal(note.Metadata)
	if err != nil {
		return "", errors.Wrapf(err, "cannot serialize note metadata to JSON: %s", note.Path)
	}
	return string(json), nil
}

// addLinks inserts all the outbound links of the given note.
func (d *NoteDAO) addLinks(id core.NoteId, note note.Metadata) error {
	for _, link := range note.Links {
		targetId, err := d.findIdByPathPrefix(link.Href)
		if err != nil {
			return err
		}

		_, err = d.addLinkStmt.Exec(id, d.idToSql(targetId), link.Title, link.Href, link.External, joinLinkRels(link.Rels), link.Snippet, link.SnippetStart, link.SnippetEnd)
		if err != nil {
			return err
		}
	}

	_, err := d.setLinksTargetStmt.Exec(int64(id), note.Path)
	return err
}

// joinLinkRels will concatenate a list of rels into a SQLite ready string.
// Each rel is delimited by \x01 for easy matching in queries.
func joinLinkRels(rels []string) string {
	if len(rels) == 0 {
		return ""
	}
	delimiter := "\x01"
	return delimiter + strings.Join(rels, delimiter) + delimiter
}

// Remove deletes the note with the given path from the index.
func (d *NoteDAO) Remove(path string) error {
	id, err := d.findIdByPath(path)
	if err != nil {
		return err
	}
	if !id.IsValid() {
		return errors.New("note not found in the index")
	}

	_, err = d.removeStmt.Exec(id)
	return err
}

func (d *NoteDAO) findIdByPath(path string) (core.NoteId, error) {
	row, err := d.findIdByPathStmt.QueryRow(path)
	if err != nil {
		return core.NoteId(0), err
	}
	return idForRow(row)
}

func (d *NoteDAO) findIdsByPathPrefixes(paths []string) ([]core.NoteId, error) {
	ids := make([]core.NoteId, 0)
	for _, path := range paths {
		id, err := d.findIdByPathPrefix(path)
		if err != nil {
			return ids, err
		}
		if id.IsValid() {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

func (d *NoteDAO) findIdByPathPrefix(path string) (core.NoteId, error) {
	row, err := d.findIdByPathPrefixStmt.QueryRow(path)
	if err != nil {
		return core.NoteId(0), err
	}
	return idForRow(row)
}

func idForRow(row *sql.Row) (core.NoteId, error) {
	var id sql.NullInt64
	err := row.Scan(&id)

	switch {
	case err == sql.ErrNoRows:
		return core.NoteId(0), nil
	case err != nil:
		return core.NoteId(0), err
	default:
		return core.NoteId(id.Int64), nil
	}
}

// Find returns all the notes matching the given criteria.
func (d *NoteDAO) Find(opts note.FinderOpts) ([]note.Match, error) {
	matches := make([]note.Match, 0)

	opts, err := d.expandMentionsIntoMatch(opts)
	if err != nil {
		return matches, err
	}

	rows, err := d.findRows(opts)
	if err != nil {
		return matches, err
	}
	defer rows.Close()

	for rows.Next() {
		var (
			id, wordCount                 int
			title, lead, body, rawContent string
			snippets, tags                sql.NullString
			path, metadataJSON, checksum  string
			created, modified             time.Time
		)

		err := rows.Scan(
			&id, &path, &title, &lead, &body, &rawContent, &wordCount,
			&created, &modified, &metadataJSON, &checksum, &tags, &snippets,
		)
		if err != nil {
			d.logger.Err(err)
			continue
		}

		metadata, err := d.unmarshalMetadata(metadataJSON)
		if err != nil {
			d.logger.Err(errors.Wrap(err, path))
		}

		matches = append(matches, note.Match{
			Snippets: parseListFromNullString(snippets),
			Metadata: note.Metadata{
				Path:       path,
				Title:      title,
				Lead:       lead,
				Body:       body,
				RawContent: rawContent,
				WordCount:  wordCount,
				Links:      []note.Link{},
				Tags:       parseListFromNullString(tags),
				Metadata:   metadata,
				Created:    created,
				Modified:   modified,
				Checksum:   checksum,
			},
		})
	}

	return matches, nil
}

// parseListFromNullString splits a 0-separated string.
func parseListFromNullString(str sql.NullString) []string {
	list := []string{}
	if str.Valid && str.String != "" {
		list = strings.Split(str.String, "\x01")
		list = strutil.RemoveDuplicates(list)
	}
	return list
}

// expandMentionsIntoMatch finds the titles associated with the notes in opts.Mention to
// expand them into the opts.Match predicate.
func (d *NoteDAO) expandMentionsIntoMatch(opts note.FinderOpts) (note.FinderOpts, error) {
	notFoundErr := fmt.Errorf("could not find notes at: " + strings.Join(opts.Mention, ","))

	if opts.Mention == nil {
		return opts, nil
	}

	// Find the IDs for the mentioned paths.
	ids, err := d.findIdsByPathPrefixes(opts.Mention)
	if err != nil {
		return opts, err
	}
	if len(ids) == 0 {
		return opts, notFoundErr
	}

	// Exclude the mentioned notes from the results.
	if opts.ExcludeIds == nil {
		opts.ExcludeIds = ids
	} else {
		for _, id := range ids {
			opts.ExcludeIds = append(opts.ExcludeIds, id)
		}
	}

	// Find their titles.
	titlesQuery := "SELECT title, metadata FROM notes WHERE id IN (" + d.joinIds(ids, ",") + ")"
	rows, err := d.tx.Query(titlesQuery)
	if err != nil {
		return opts, err
	}
	defer rows.Close()

	titles := []string{}

	appendTitle := func(t string) {
		titles = append(titles, `"`+strings.ReplaceAll(t, `"`, "")+`"`)
	}

	for rows.Next() {
		var title, metadataJSON string
		err := rows.Scan(&title, &metadataJSON)
		if err != nil {
			return opts, err
		}

		appendTitle(title)

		// Support `aliases` key in the YAML frontmatter, like Obsidian:
		// https://publish.obsidian.md/help/How+to/Add+aliases+to+note
		metadata, err := d.unmarshalMetadata(metadataJSON)
		if err != nil {
			d.logger.Err(err)
		} else {
			if aliases, ok := metadata["aliases"]; ok {
				switch aliases := aliases.(type) {
				case []interface{}:
					for _, alias := range aliases {
						appendTitle(fmt.Sprint(alias))
					}
				case string:
					appendTitle(aliases)
				}
			}
		}
	}

	if len(titles) == 0 {
		return opts, notFoundErr
	}

	// Expand the titles in the match predicate.
	match := opts.Match.String()
	match += " (" + strings.Join(titles, " OR ") + ")"
	opts.Match = opt.NewString(match)

	return opts, nil
}

func (d *NoteDAO) findRows(opts note.FinderOpts) (*sql.Rows, error) {
	snippetCol := `n.lead`
	joinClauses := []string{}
	whereExprs := []string{}
	additionalOrderTerms := []string{}
	args := []interface{}{}
	groupBy := ""

	transitiveClosure := false
	maxDistance := 0

	setupLinkFilter := func(paths []string, direction int, negate, recursive bool) error {
		ids, err := d.findIdsByPathPrefixes(paths)
		if err != nil {
			return err
		}
		if len(ids) == 0 {
			return nil
		}
		idsList := "(" + d.joinIds(ids, ",") + ")"

		linksSrc := "links"

		if recursive {
			transitiveClosure = true
			linksSrc = "transitive_closure"
		}

		if !negate {
			if direction != 0 {
				snippetCol = "GROUP_CONCAT(REPLACE(l.snippet, l.title, '<zk:match>' || l.title || '</zk:match>'), '\x01')"
			}

			joinOns := make([]string, 0)
			if direction <= 0 {
				joinOns = append(joinOns, fmt.Sprintf(
					"(n.id = l.target_id AND l.source_id IN %s)", idsList,
				))
			}
			if direction >= 0 {
				joinOns = append(joinOns, fmt.Sprintf(
					"(n.id = l.source_id AND l.target_id IN %s)", idsList,
				))
			}

			joinClauses = append(joinClauses, fmt.Sprintf(
				"LEFT JOIN %s l ON %s",
				linksSrc,
				strings.Join(joinOns, " OR "),
			))

			groupBy = "GROUP BY n.id"
		}

		idExpr := "n.id"
		if negate {
			idExpr += " NOT"
		}

		idSelects := make([]string, 0)
		if direction <= 0 {
			idSelects = append(idSelects, fmt.Sprintf(
				"    SELECT target_id FROM %s WHERE target_id IS NOT NULL AND source_id IN %s",
				linksSrc, idsList,
			))
		}
		if direction >= 0 {
			idSelects = append(idSelects, fmt.Sprintf(
				"    SELECT source_id FROM %s WHERE target_id IS NOT NULL AND target_id IN %s",
				linksSrc, idsList,
			))
		}

		idExpr += " IN (\n" + strings.Join(idSelects, "\n    UNION\n") + "\n)"

		whereExprs = append(whereExprs, idExpr)

		return nil
	}

	if !opts.Match.IsNull() {
		snippetCol = `snippet(notes_fts, 2, '<zk:match>', '</zk:match>', '…', 20)`
		joinClauses = append(joinClauses, "JOIN notes_fts ON n.id = notes_fts.rowid")
		additionalOrderTerms = append(additionalOrderTerms, `bm25(notes_fts, 1000.0, 500.0, 1.0)`)
		whereExprs = append(whereExprs, "notes_fts MATCH ?")
		args = append(args, fts5.ConvertQuery(opts.Match.String()))
	}

	if opts.IncludePaths != nil {
		regexes := make([]string, 0)
		for _, path := range opts.IncludePaths {
			regexes = append(regexes, "n.path REGEXP ?")
			args = append(args, pathRegex(path))
		}
		whereExprs = append(whereExprs, strings.Join(regexes, " OR "))
	}

	if opts.ExcludePaths != nil {
		regexes := make([]string, 0)
		for _, path := range opts.ExcludePaths {
			regexes = append(regexes, "n.path NOT REGEXP ?")
			args = append(args, pathRegex(path))
		}
		whereExprs = append(whereExprs, strings.Join(regexes, " AND "))
	}

	if opts.ExcludeIds != nil {
		whereExprs = append(whereExprs, "n.id NOT IN ("+d.joinIds(opts.ExcludeIds, ",")+")")
	}

	if opts.Tags != nil {
		separatorRegex := regexp.MustCompile(`(\ OR\ )|\|`)
		for _, tagsArg := range opts.Tags {
			tags := separatorRegex.Split(tagsArg, -1)

			negate := false
			globs := make([]string, 0)
			for _, tag := range tags {
				tag = strings.TrimSpace(tag)

				if strings.HasPrefix(tag, "-") {
					negate = true
					tag = strings.TrimPrefix(tag, "-")
				} else if strings.HasPrefix(tag, "NOT") {
					negate = true
					tag = strings.TrimPrefix(tag, "NOT")
				}

				tag = strings.TrimSpace(tag)
				if len(tag) == 0 {
					continue
				}
				globs = append(globs, "t.name GLOB ?")
				args = append(args, tag)
			}

			if len(globs) == 0 {
				continue
			}
			if negate && len(globs) > 1 {
				return nil, fmt.Errorf("cannot negate a tag in a OR group: %s", tagsArg)
			}

			expr := "n.id"
			if negate {
				expr += " NOT"
			}
			expr += fmt.Sprintf(` IN (
SELECT note_id FROM notes_collections
WHERE collection_id IN (SELECT id FROM collections t WHERE kind = '%s' AND (%s))
)`,
				note.CollectionKindTag,
				strings.Join(globs, " OR "),
			)
			whereExprs = append(whereExprs, expr)
		}
	}

	if opts.LinkedBy != nil {
		filter := opts.LinkedBy
		maxDistance = filter.MaxDistance
		err := setupLinkFilter(filter.Paths, -1, filter.Negate, filter.Recursive)
		if err != nil {
			return nil, err
		}
	}

	if opts.LinkTo != nil {
		filter := opts.LinkTo
		maxDistance = filter.MaxDistance
		err := setupLinkFilter(filter.Paths, 1, filter.Negate, filter.Recursive)
		if err != nil {
			return nil, err
		}
	}

	if opts.Related != nil {
		maxDistance = 2
		err := setupLinkFilter(opts.Related, 0, false, true)
		if err != nil {
			return nil, err
		}
		groupBy += " HAVING MIN(l.distance) = 2"
	}

	if opts.Orphan {
		whereExprs = append(whereExprs, `n.id NOT IN (
			SELECT target_id FROM links WHERE target_id IS NOT NULL
		)`)
	}

	if opts.CreatedStart != nil {
		whereExprs = append(whereExprs, "created >= ?")
		args = append(args, opts.CreatedStart)
	}

	if opts.CreatedEnd != nil {
		whereExprs = append(whereExprs, "created < ?")
		args = append(args, opts.CreatedEnd)
	}

	if opts.ModifiedStart != nil {
		whereExprs = append(whereExprs, "modified >= ?")
		args = append(args, opts.ModifiedStart)
	}

	if opts.ModifiedEnd != nil {
		whereExprs = append(whereExprs, "modified < ?")
		args = append(args, opts.ModifiedEnd)
	}

	orderTerms := []string{}
	for _, sorter := range opts.Sorters {
		orderTerms = append(orderTerms, orderTerm(sorter))
	}
	orderTerms = append(orderTerms, additionalOrderTerms...)
	orderTerms = append(orderTerms, `n.title ASC`)

	query := ""

	// Credit to https://inviqa.com/blog/storing-graphs-database-sql-meets-social-network
	if transitiveClosure {
		orderTerms = append([]string{"l.distance"}, orderTerms...)

		query += `WITH RECURSIVE transitive_closure(source_id, target_id, title, snippet, distance, path) AS (
    SELECT source_id, target_id, title, snippet,
           1 AS distance,
           '.' || source_id || '.' || target_id || '.' AS path
      FROM links
 
     UNION ALL
 
    SELECT tc.source_id, l.target_id, l.title, l.snippet,
           tc.distance + 1,
           tc.path || l.target_id || '.' AS path
      FROM links AS l
      JOIN transitive_closure AS tc
        ON l.source_id = tc.target_id
     WHERE tc.path NOT LIKE '%.' || l.target_id || '.%'`

		if maxDistance != 0 {
			query += fmt.Sprintf(" AND tc.distance < %d", maxDistance)
		}

		// Guard against infinite loops by limiting the number of recursions.
		query += "\n     LIMIT 100000"

		query += "\n)\n"
	}

	query += fmt.Sprintf("SELECT n.id, n.path, n.title, n.lead, n.body, n.raw_content, n.word_count, n.created, n.modified, n.metadata, n.checksum, n.tags, %s AS snippet\n", snippetCol)

	query += "FROM notes_with_metadata n\n"

	for _, clause := range joinClauses {
		query += clause + "\n"
	}

	if len(whereExprs) > 0 {
		query += "WHERE " + strings.Join(whereExprs, "\nAND ") + "\n"
	}

	if groupBy != "" {
		query += groupBy + "\n"
	}

	query += "ORDER BY " + strings.Join(orderTerms, ", ") + "\n"

	if opts.Limit > 0 {
		query += fmt.Sprintf("LIMIT %d\n", opts.Limit)
	}

	// fmt.Println(query)
	// fmt.Println(args)
	return d.tx.Query(query, args...)
}

func orderTerm(sorter note.Sorter) string {
	order := " ASC"
	if !sorter.Ascending {
		order = " DESC"
	}

	switch sorter.Field {
	case note.SortCreated:
		return "n.created" + order
	case note.SortModified:
		return "n.modified" + order
	case note.SortPath:
		return "n.path" + order
	case note.SortRandom:
		return "RANDOM()"
	case note.SortTitle:
		return "n.title" + order
	case note.SortWordCount:
		return "n.word_count" + order
	default:
		panic(fmt.Sprintf("%v: unknown note.SortField", sorter.Field))
	}
}

// pathRegex returns an ICU regex to match the files in the folder at given
// `path`, or any file having `path` for prefix.
func pathRegex(path string) string {
	path = icu.EscapePattern(path)
	return path + "[^/]*|" + path + "/.+"
}

func (d *NoteDAO) idToSql(id core.NoteId) sql.NullInt64 {
	if id.IsValid() {
		return sql.NullInt64{Int64: int64(id), Valid: true}
	} else {
		return sql.NullInt64{}
	}
}

func (d *NoteDAO) joinIds(ids []core.NoteId, delimiter string) string {
	strs := make([]string, 0)
	for _, i := range ids {
		strs = append(strs, strconv.FormatInt(int64(i), 10))
	}
	return strings.Join(strs, delimiter)
}

func (d *NoteDAO) unmarshalMetadata(metadataJSON string) (metadata map[string]interface{}, err error) {
	err = json.Unmarshal([]byte(metadataJSON), &metadata)
	err = errors.Wrapf(err, "cannot parse note metadata from JSON: %s", metadataJSON)
	return
}
