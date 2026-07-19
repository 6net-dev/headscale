defmodule RegistrarWeb.Router do
  use RegistrarWeb, :router

  pipeline :api do
    plug :accepts, ["json"]
  end

  scope "/api", RegistrarWeb do
    pipe_through :api

    get "/world/:node_key", WorldController, :show
    post "/devices", DeviceController, :create
    put "/devices/:node_key/grants/:org", GrantController, :update
    get "/devices/:node_key/grants", GrantController, :index
    post "/devices/:node_key/grants", AdmissionController, :create
  end

  # SSE: no :accepts pipeline — the stream negotiates its own
  # content type (text/event-stream).
  scope "/api", RegistrarWeb do
    get "/changes", ChangesController, :stream
  end
end
