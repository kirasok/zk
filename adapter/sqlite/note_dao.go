package sqlite

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/mickael-menu/zk/core/note"
	"github.com/mickael-menu/zk/util"
	"github.com/mickael-menu/zk/util/errors"
	"github.com/mickael-menu/zk/util/fts5"
	"github.com/mickael-menu/zk/util/paths"
	strutil "github.com/mickael-menu/zk/util/strings"
)

// NoteDAO persists notes in the SQLite database.
// It implements the core ports note.Indexer and note.Finder.
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
			INSERT INTO notes (path, sortable_path, title, lead, body, raw_content, word_count, checksum, created, modified)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		`),

		// Update the content of a note.
		updateStmt: tx.PrepareLazy(`
			UPDATE notes
			   SET title = ?, lead = ?, body = ?, raw_content = ?, word_count = ?, checksum = ?, modified = ?
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
			INSERT INTO links (source_id, target_id, title, href, external, rels, snippet)
			VALUES (?, ?, ?, ?, ?, ?, ?)
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
	wrap := errors.Wrapper("failed to get indexed notes")

	rows, err := d.indexedStmt.Query()
	if err != nil {
		return nil, wrap(err)
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
				d.logger.Err(wrap(err))
			}

			c <- paths.Metadata{
				Path:     path,
				Modified: modified,
			}
		}

		err = rows.Err()
		if err != nil {
			d.logger.Err(wrap(err))
		}
	}()

	return c, nil
}

// Add inserts a new note to the index.
func (d *NoteDAO) Add(note note.Metadata) (int64, error) {
	wrap := errors.Wrapperf("%v: can't add note to the index", note.Path)

	// For sortable_path, we replace in path / by the shortest non printable
	// character available to make it sortable. Without this, sorting by the
	// path would be a lexicographical sort instead of being the same order
	// returned by filepath.Walk.
	// \x01 is used instead of \x00, because SQLite treats \x00 as and end of
	// string.
	sortablePath := strings.ReplaceAll(note.Path, "/", "\x01")

	res, err := d.addStmt.Exec(
		note.Path, sortablePath, note.Title, note.Lead, note.Body, note.RawContent, note.WordCount, note.Checksum,
		note.Created, note.Modified,
	)
	if err != nil {
		return 0, wrap(err)
	}

	id, err := res.LastInsertId()
	if err != nil {
		return 0, wrap(err)
	}

	err = d.addLinks(id, note)
	return id, err
}

// Update modifies an existing note.
func (d *NoteDAO) Update(note note.Metadata) error {
	wrap := errors.Wrapperf("%v: failed to update note index", note.Path)

	id, err := d.findIdByPath(note.Path)
	if err != nil {
		return wrap(err)
	}
	if !id.Valid {
		return wrap(errors.New("note not found in the index"))
	}

	_, err = d.updateStmt.Exec(
		note.Title, note.Lead, note.Body, note.RawContent, note.WordCount, note.Checksum, note.Modified,
		note.Path,
	)
	if err != nil {
		return wrap(err)
	}

	_, err = d.removeLinksStmt.Exec(id.Int64)
	if err != nil {
		return wrap(err)
	}

	err = d.addLinks(id.Int64, note)
	return wrap(err)
}

// addLinks inserts all the outbound links of the given note.
func (d *NoteDAO) addLinks(id int64, note note.Metadata) error {
	for _, link := range note.Links {
		targetId, err := d.findIdByPathPrefix(link.Href)
		if err != nil {
			return err
		}

		_, err = d.addLinkStmt.Exec(id, targetId, link.Title, link.Href, link.External, joinLinkRels(link.Rels), link.Snippet)
		if err != nil {
			return err
		}
	}

	_, err := d.setLinksTargetStmt.Exec(id, note.Path)
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
	wrap := errors.Wrapperf("%v: failed to remove note index", path)

	id, err := d.findIdByPath(path)
	if err != nil {
		return wrap(err)
	}
	if !id.Valid {
		return wrap(errors.New("note not found in the index"))
	}

	_, err = d.removeStmt.Exec(id)
	return wrap(err)
}

func (d *NoteDAO) findIdByPath(path string) (sql.NullInt64, error) {
	row, err := d.findIdByPathStmt.QueryRow(path)
	if err != nil {
		return sql.NullInt64{}, err
	}
	return idForRow(row)
}

func (d *NoteDAO) findIdsByPathPrefixes(paths []string) ([]int64, error) {
	ids := make([]int64, 0)
	for _, path := range paths {
		id, err := d.findIdByPathPrefix(path)
		if err != nil {
			return ids, err
		}
		if id.Valid {
			ids = append(ids, id.Int64)
		}
	}
	return ids, nil
}

func (d *NoteDAO) findIdByPathPrefix(path string) (sql.NullInt64, error) {
	row, err := d.findIdByPathPrefixStmt.QueryRow(path)
	if err != nil {
		return sql.NullInt64{}, err
	}
	return idForRow(row)
}

func idForRow(row *sql.Row) (sql.NullInt64, error) {
	var id sql.NullInt64
	err := row.Scan(&id)

	switch {
	case err == sql.ErrNoRows:
		return id, nil
	case err != nil:
		return id, err
	default:
		return id, err
	}
}

// Find returns all the notes matching the given criteria.
func (d *NoteDAO) Find(opts note.FinderOpts) ([]note.Match, error) {
	matches := make([]note.Match, 0)

	rows, err := d.findRows(opts)
	if err != nil {
		return matches, err
	}
	defer rows.Close()

	for rows.Next() {
		var (
			id, wordCount                 int
			title, lead, body, rawContent string
			nullableSnippets              sql.NullString
			path, checksum                string
			created, modified             time.Time
		)

		err := rows.Scan(&id, &path, &title, &lead, &body, &rawContent, &wordCount, &created, &modified, &checksum, &nullableSnippets)
		if err != nil {
			d.logger.Err(err)
			continue
		}

		snippets := make([]string, 0)
		if nullableSnippets.Valid && nullableSnippets.String != "" {
			snippets = strings.Split(nullableSnippets.String, "\x01")
			snippets = strutil.RemoveDuplicates(snippets)
		}

		matches = append(matches, note.Match{
			Snippets: snippets,
			Metadata: note.Metadata{
				Path:       path,
				Title:      title,
				Lead:       lead,
				Body:       body,
				RawContent: rawContent,
				WordCount:  wordCount,
				Created:    created,
				Modified:   modified,
				Checksum:   checksum,
			},
		})
	}

	return matches, nil
}

func (d *NoteDAO) findRows(opts note.FinderOpts) (*sql.Rows, error) {
	snippetCol := `n.lead`
	cteClauses := make([]string, 0)
	joinClauses := make([]string, 0)
	whereExprs := make([]string, 0)
	orderTerms := make([]string, 0)
	groupById := false
	args := make([]interface{}, 0)

	setupLinkFilter := func(paths []string, forward, negate bool, recursive bool) error {
		ids, err := d.findIdsByPathPrefixes(paths)
		if err != nil {
			return err
		}
		if len(ids) == 0 {
			return nil
		}
		idsList := "(" + strutil.JoinInt64(ids, ",") + ")"

		alias := "l"
		if forward {
			alias += "f"
		}
		if negate {
			alias += "n"
		}

		links_src := "links"
		from := "source_id"
		to := "target_id"
		if !forward {
			from, to = to, from
		}

		if recursive {
			// Credit to https://inviqa.com/blog/storing-graphs-database-sql-meets-social-network
			links_src = "links_transitive_closure"
			orderTerms = append(orderTerms, alias+".distance")
			cteClauses = append(cteClauses, `WITH RECURSIVE links_transitive_closure(source_id, target_id, title, snippet, distance, path) AS (
    SELECT source_id, target_id, title, snippet,
	       1 AS distance,
           source_id || '.' || target_id || '.' AS path
      FROM links
 
     UNION ALL
 
    SELECT tc.source_id, l.target_id, l.title, l.snippet,
	       tc.distance + 1,
           tc.path || l.target_id || '.' AS path
      FROM links AS l
      JOIN links_transitive_closure AS tc
        ON l.source_id = tc.target_id
     WHERE tc.path NOT LIKE '%' || l.target_id || '.%'
)`)
		}

		if !negate {
			groupById = true
			joinClauses = append(joinClauses, fmt.Sprintf(`LEFT JOIN %[1]s %[2]s ON n.id = %[2]s.%[3]s AND %[2]s.%[4]s IN %[5]s`, links_src, alias, from, to, idsList))
			snippetCol = fmt.Sprintf("GROUP_CONCAT(REPLACE(%[1]s.snippet, %[1]s.title, '<zk:match>' || %[1]s.title || '</zk:match>'), '\x01') AS snippet", alias)
		}

		expr := "n.id"
		if negate {
			expr += " NOT"
		}
		expr += fmt.Sprintf(" IN (SELECT %[2]s FROM %[1]s WHERE target_id IS NOT NULL AND %[3]s IN %[4]s)", links_src, from, to, idsList)

		whereExprs = append(whereExprs, expr)

		return nil
	}

	for _, filter := range opts.Filters {
		switch filter := filter.(type) {

		case note.MatchFilter:
			snippetCol = `snippet(notes_fts, 2, '<zk:match>', '</zk:match>', '…', 20) as snippet`
			joinClauses = append(joinClauses, "JOIN notes_fts ON n.id = notes_fts.rowid")
			orderTerms = append(orderTerms, `bm25(notes_fts, 1000.0, 500.0, 1.0)`)
			whereExprs = append(whereExprs, "notes_fts MATCH ?")
			args = append(args, fts5.ConvertQuery(string(filter)))

		case note.PathFilter:
			if len(filter) == 0 {
				break
			}
			globs := make([]string, 0)
			for _, path := range filter {
				globs = append(globs, "n.path GLOB ?")
				args = append(args, path+"*")
			}
			whereExprs = append(whereExprs, strings.Join(globs, " OR "))

		case note.ExcludePathFilter:
			if len(filter) == 0 {
				break
			}
			globs := make([]string, 0)
			for _, path := range filter {
				globs = append(globs, "n.path NOT GLOB ?")
				args = append(args, path+"*")
			}
			whereExprs = append(whereExprs, strings.Join(globs, " AND "))

		case note.LinkedByFilter:
			err := setupLinkFilter(filter.Paths, false, filter.Negate, filter.Recursive)
			if err != nil {
				return nil, err
			}

		case note.LinkingToFilter:
			err := setupLinkFilter(filter.Paths, true, filter.Negate, filter.Recursive)
			if err != nil {
				return nil, err
			}

		case note.OrphanFilter:
			whereExprs = append(whereExprs, `n.id NOT IN (
				SELECT target_id FROM links WHERE target_id IS NOT NULL
			)`)

		case note.DateFilter:
			value := "?"
			field := "n." + dateField(filter)
			op, ignoreTime := dateDirection(filter)
			if ignoreTime {
				field = "date(" + field + ")"
				value = "date(?)"
			}

			whereExprs = append(whereExprs, fmt.Sprintf("%s %s %s", field, op, value))
			args = append(args, filter.Date)

		case note.InteractiveFilter:
			// No user interaction possible from here.
			break

		default:
			panic(fmt.Sprintf("%v: unknown filter type", filter))
		}
	}

	for _, sorter := range opts.Sorters {
		orderTerms = append(orderTerms, orderTerm(sorter))
	}
	orderTerms = append(orderTerms, `n.title ASC`)

	query := ""

	for _, clause := range cteClauses {
		query += clause + "\n"
	}

	query += "SELECT n.id, n.path, n.title, n.lead, n.body, n.raw_content, n.word_count, n.created, n.modified, n.checksum, " + snippetCol + "\n"

	query += "FROM notes n\n"

	for _, clause := range joinClauses {
		query += clause + "\n"
	}

	if len(whereExprs) > 0 {
		query += "WHERE " + strings.Join(whereExprs, "\nAND ") + "\n"
	}

	if groupById {
		query += "GROUP BY n.id\n"
	}

	query += "ORDER BY " + strings.Join(orderTerms, ", ") + "\n"

	if opts.Limit > 0 {
		query += fmt.Sprintf("LIMIT %d\n", opts.Limit)
	}

	// fmt.Println(query)
	// fmt.Println(args)
	return d.tx.Query(query, args...)
}

func dateField(filter note.DateFilter) string {
	switch filter.Field {
	case note.DateCreated:
		return "created"
	case note.DateModified:
		return "modified"
	default:
		panic(fmt.Sprintf("%v: unknown note.DateField", filter.Field))
	}
}

func dateDirection(filter note.DateFilter) (op string, ignoreTime bool) {
	switch filter.Direction {
	case note.DateOn:
		return "=", true
	case note.DateBefore:
		return "<", false
	case note.DateAfter:
		return ">=", false
	default:
		panic(fmt.Sprintf("%v: unknown note.DateDirection", filter.Direction))
	}
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
