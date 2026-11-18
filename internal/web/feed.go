package web

import (
	"net/http"
	"net/url"
	"strconv"

	"github.com/forgeutah/forge-forum/internal/forum"
)

// pageSize is how many threads one feed page shows.
const pageSize = forum.DefaultPageSize

// maxPage keeps a typed-in page number from turning into a huge offset.
const maxPage = 100000

type tagLink struct {
	Slug   string
	URL    string
	Active bool
}

type threadRow struct {
	forum.Thread
	URL  string
	Vote voteView
}

type feedView struct {
	Page
	// SortTop is true under "most upvoted", false under "recent activity".
	SortTop     bool
	ActivityURL string
	TopURL      string

	Tag string
	// AllURL clears the tag filter; FirstURL is page one of this view.
	AllURL   string
	FirstURL string
	Tags     []tagLink
	Threads  []threadRow

	PageNum int
	// NewerURL and OlderURL are empty when there is no such page.
	NewerURL string
	OlderURL string
}

// feedURL builds a feed address, leaving out defaults so the plain feed is
// always just "/".
func feedURL(tag string, top bool, page int) string {
	q := url.Values{}
	if tag != "" {
		q.Set("tag", tag)
	}
	if top {
		q.Set("sort", "top")
	}
	if page > 1 {
		q.Set("page", strconv.Itoa(page))
	}
	if len(q) == 0 {
		return "/"
	}
	return "/?" + q.Encode()
}

func threadURL(id int64) string { return "/threads/" + strconv.FormatInt(id, 10) }

func (s *Server) handleFeed(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	tag := q.Get("tag")
	top := q.Get("sort") == "top"
	page, err := strconv.Atoi(q.Get("page"))
	if err != nil || page < 1 {
		page = 1
	}
	page = min(page, maxPage)

	opts := forum.ListOptions{Tag: tag, Page: page, PageSize: pageSize, ViewerID: member(r).ID}
	if top {
		opts.Sort = forum.SortScore
	}
	threads, hasMore, err := s.store.ListThreads(r.Context(), opts)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	active, err := s.store.ActiveTags(r.Context())
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	view := feedView{
		Page:        s.page(r, "Threads", "feed"),
		SortTop:     top,
		ActivityURL: feedURL(tag, false, 1),
		TopURL:      feedURL(tag, true, 1),
		Tag:         tag,
		AllURL:      feedURL("", top, 1),
		FirstURL:    feedURL(tag, top, 1),
		PageNum:     page,
	}
	for _, slug := range active {
		view.Tags = append(view.Tags, tagLink{Slug: slug, URL: feedURL(slug, top, 1), Active: slug == tag})
	}
	here := feedURL(tag, top, page)
	for _, t := range threads {
		row := threadRow{Thread: t, URL: threadURL(t.ID)}
		row.Vote = s.voteView(member(r), row.URL+"/vote", "vote", t.Score, t.Voted)
		// Voting from the feed comes back to the same page, at the same row.
		row.Vote.Return = here + "#thread-" + strconv.FormatInt(t.ID, 10)
		view.Threads = append(view.Threads, row)
	}
	if page > 1 {
		view.NewerURL = feedURL(tag, top, page-1)
	}
	if hasMore {
		view.OlderURL = feedURL(tag, top, page+1)
	}
	s.render(w, r, http.StatusOK, "feed", view)
}
