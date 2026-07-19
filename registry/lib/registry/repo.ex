defmodule Registrar.Repo do
  use Ecto.Repo,
    otp_app: :registry,
    adapter: Ecto.Adapters.Postgres
end
