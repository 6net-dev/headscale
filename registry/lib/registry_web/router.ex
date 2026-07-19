defmodule RegistrarWeb.Router do
  use RegistrarWeb, :router

  pipeline :api do
    plug :accepts, ["json"]
  end

  scope "/api", RegistrarWeb do
    pipe_through :api

    get "/world/:node_key", WorldController, :show
  end
end
