defmodule Registrar.Orgs.Slug do
  @moduledoc """
  Org slug rules. A slug is not branding: it is a DNS label in every
  member's resolver (`<device>.<slug>.<base-domain>`), so it must be a
  strict RFC 1123 label — lowercase ASCII alphanumerics and interior
  hyphens, 2..63 bytes — and must not collide with infrastructure
  names.
  """

  @reserved ~w(
    www api admin derp dns registry internal mail ns ftp vpn sso auth
    login logout status docs help support billing root system
  )

  @syntax ~r/^[a-z0-9]([a-z0-9-]*[a-z0-9])?$/

  def valid?(slug) when is_binary(slug) do
    byte_size(slug) in 2..63 and
      Regex.match?(@syntax, slug) and
      slug not in @reserved
  end

  def valid?(_), do: false

  def reserved, do: @reserved
end
