package web

import (
	"errors"
	"net/http"

	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/store"
)

// graphForPage는 등급 확인 뒤 읽은 그래프의 부재와 저장 장애를 구분하고 요청 대상을 재검사한다.
func (server *Server) graphForPage(writer http.ResponseWriter, request *http.Request, graphID model.ID, title string) (model.Graph, bool) {
	graph, err := server.graphs.Graph(request.Context(), graphID)
	if request.Context().Err() != nil {
		return model.Graph{}, false
	}
	if err != nil {
		status, message := http.StatusInternalServerError, "그래프를 읽을 수 없습니다."
		if errors.Is(err, store.ErrNotFound) {
			status, message = http.StatusNotFound, "그래프를 찾을 수 없습니다."
		}
		server.render(writer, status, "message", pageData{Title: title, Error: message})
		return model.Graph{}, false
	}
	if graph.ID != graphID {
		server.responseScopeError(writer)
		return model.Graph{}, false
	}
	return graph, true
}

func (server *Server) responseScopeError(writer http.ResponseWriter) {
	server.render(writer, http.StatusInternalServerError, "message", pageData{Title: "조회 오류", Error: "조회 결과를 표시할 수 없습니다."})
}

func contextsInGraph(graphID model.ID, contexts []model.Context) bool {
	for _, value := range contexts {
		if value.GraphID != graphID {
			return false
		}
	}
	return true
}

// 간선에는 graph_id가 없으므로 양 끝이 같은 요청 그래프의 반환 정점 집합 안에 있어야 한다.
func visualizationInGraph(graphID model.ID, result store.HopResult) bool {
	if !contextsInGraph(graphID, result.Contexts) {
		return false
	}
	nodes := make(map[model.ID]struct{}, len(result.Contexts))
	for _, value := range result.Contexts {
		if value.DeletedAt != nil {
			return false
		}
		nodes[value.ID] = struct{}{}
	}
	for _, edge := range result.Edges {
		if _, ok := nodes[edge.FromID]; !ok {
			return false
		}
		if _, ok := nodes[edge.ToID]; !ok {
			return false
		}
	}
	return true
}
