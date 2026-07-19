defmodule RegistrarWeb.ChangesController do
  use RegistrarWeb, :controller

  @moduledoc """
  Server-sent events stream of world changes, consumed by the wire
  head. Each event carries the node keys whose worlds changed; the
  head reacts by pushing fresh netmaps.
  """

  @keepalive_ms 25_000

  def stream(conn, _params) do
    :ok = Phoenix.PubSub.subscribe(Registrar.PubSub, Registrar.Grants.world_changes_topic())

    conn
    |> put_resp_header("content-type", "text/event-stream")
    |> put_resp_header("cache-control", "no-cache")
    |> send_chunked(200)
    |> loop()
  end

  defp loop(conn) do
    receive do
      {:world_changed, node_keys} ->
        case chunk(conn, "data: " <> Jason.encode!(%{node_keys: node_keys}) <> "\n\n") do
          {:ok, conn} -> loop(conn)
          {:error, _closed} -> conn
        end
    after
      @keepalive_ms ->
        case chunk(conn, ": keepalive\n\n") do
          {:ok, conn} -> loop(conn)
          {:error, _closed} -> conn
        end
    end
  end
end
